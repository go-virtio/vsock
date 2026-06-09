// End-to-end tests for the OpenVirtioVsock driver path plus the
// packet Send/Receive path. fakeVsockDevice is a minimal in-memory
// virtio-vsock device that:
//
//   - Publishes a valid cap chain (CommonCfg + extended NotifyCfg +
//     DeviceCfg holding the le64 guest_cid).
//   - Tracks COMMON_CFG state across the three virtqueues.
//   - Completes TX descriptors on a tx-queue doorbell (handleTxComplete).
//   - Injects inbound packets on demand via deliverRaw (the device side
//     of the rx path).
//
// injectTransport forces a transport-level error (or a zero / count-gated
// physical address) so every error-return branch is reachable.

package vsock

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"github.com/go-virtio/common"
)

var le = binary.LittleEndian

type fakeVsockDevice struct {
	mu sync.Mutex

	cfg []byte

	deviceFeatureSelect uint32
	deviceFeatures      uint64
	driverFeatures      uint64
	deviceStatus        uint8
	currentQueue        uint16

	qsize      map[uint16]uint16
	qenable    map[uint16]uint16
	qdesc      map[uint16]uint64
	qdriver    map[uint16]uint64
	qdevice    map[uint16]uint64
	qnotifyOff map[uint16]uint16

	bar map[uint64]uint64

	guestCID        uint64
	clearFeaturesOK bool
	txCompletes     bool

	// rxConsumed is the device's running index into the rx avail ring
	// (how many rx buffers it has filled).
	rxConsumed uint16

	heldPages [][]byte
	allocFail bool
}

func newFakeVsockDevice(deviceFeats uint64, cid uint64) *fakeVsockDevice {
	d := &fakeVsockDevice{
		deviceFeatures: deviceFeats,
		qsize:          map[uint16]uint16{0: 32, 1: 32, 2: 32},
		qenable:        map[uint16]uint16{},
		qdesc:          map[uint16]uint64{},
		qdriver:        map[uint16]uint64{},
		qdevice:        map[uint16]uint64{},
		qnotifyOff:     map[uint16]uint16{0: 0, 1: 1, 2: 2},
		bar:            map[uint64]uint64{},
		guestCID:       cid,
		txCompletes:    true,
	}
	d.cfg = buildVirtioVsockCfgSpace()
	return d
}

func barKey(bar uint8, off uint64) uint64 { return uint64(bar)<<48 | off }

// PCIConfigReader.
func (d *fakeVsockDevice) ReadConfig8(off uint8) (uint8, error) {
	if int(off) >= len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return d.cfg[off], nil
}
func (d *fakeVsockDevice) ReadConfig16(off uint8) (uint16, error) {
	if int(off)+2 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint16(d.cfg[off : off+2]), nil
}
func (d *fakeVsockDevice) ReadConfig32(off uint8) (uint32, error) {
	if int(off)+4 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint32(d.cfg[off : off+4]), nil
}

// PageAllocator.
func (d *fakeVsockDevice) AllocatePages(count int) (uint64, []byte, error) {
	if d.allocFail {
		return 0, nil, errors.New("alloc fail")
	}
	mem := make([]byte, count*int(common.PageSize))
	addr := uintptr(0)
	if len(mem) > 0 {
		d.heldPages = append(d.heldPages, mem)
		addr = uintptrFromSlice(mem)
	}
	return uint64(addr), mem, nil
}

func (d *fakeVsockDevice) commonCfgBAR() uint8     { return 0 }
func (d *fakeVsockDevice) commonCfgOffset() uint64 { return 0 }

const deviceCfgOff = 0x8000

// BARMemoryAccessor.
func (d *fakeVsockDevice) Read8(bar uint8, off uint64) (uint8, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceStatus:
			return d.deviceStatus, nil
		case common.CfgConfigGeneration:
			return 0, nil
		}
	}
	return uint8(d.bar[barKey(bar, off)] & 0xFF), nil
}

func (d *fakeVsockDevice) Read16(bar uint8, off uint64) (uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgNumQueues:
			return 3, nil
		case common.CfgQueueSelect:
			return d.currentQueue, nil
		case common.CfgQueueSize:
			return d.qsize[d.currentQueue], nil
		case common.CfgQueueEnable:
			return d.qenable[d.currentQueue], nil
		case common.CfgQueueNotifyOff:
			return d.qnotifyOff[d.currentQueue], nil
		}
	}
	return uint16(d.bar[barKey(bar, off)] & 0xFFFF), nil
}

func (d *fakeVsockDevice) Read32(bar uint8, off uint64) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			return d.deviceFeatureSelect, nil
		case common.CfgDeviceFeature:
			if d.deviceFeatureSelect == 0 {
				return uint32(d.deviceFeatures & 0xFFFFFFFF), nil
			}
			return uint32(d.deviceFeatures >> 32), nil
		}
	}
	return uint32(d.bar[barKey(bar, off)] & 0xFFFFFFFF), nil
}

func (d *fakeVsockDevice) Read64(bar uint8, off uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			return d.qdesc[d.currentQueue], nil
		case common.CfgQueueDriver:
			return d.qdriver[d.currentQueue], nil
		case common.CfgQueueDevice:
			return d.qdevice[d.currentQueue], nil
		}
	}
	// DeviceCfg: le64 guest_cid at offset 0 of the device-config region.
	if bar == 0 && off >= deviceCfgOff && off < deviceCfgOff+8 {
		return d.guestCID, nil
	}
	return d.bar[barKey(bar, off)], nil
}

func (d *fakeVsockDevice) Write8(bar uint8, off uint64, v uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() && off-d.commonCfgOffset() == common.CfgDeviceStatus {
		if v&common.StatusFeaturesOK != 0 {
			if d.clearFeaturesOK || d.driverFeatures&common.FeatureVersion1 == 0 {
				v &^= common.StatusFeaturesOK
			}
		}
		d.deviceStatus = v
		return nil
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeVsockDevice) Write16(bar uint8, off uint64, v uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueSelect:
			d.currentQueue = v
			return nil
		case common.CfgQueueSize:
			d.qsize[d.currentQueue] = v
			return nil
		case common.CfgQueueEnable:
			d.qenable[d.currentQueue] = v
			return nil
		}
	}
	if off >= 0x1000 && off < 0x2000 {
		d.handleNotify(v)
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeVsockDevice) Write32(bar uint8, off uint64, v uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			d.deviceFeatureSelect = v
			return nil
		case common.CfgDriverFeatureSelect:
			d.bar[barKey(bar, off)] = uint64(v)
			return nil
		case common.CfgDriverFeature:
			sel := d.bar[barKey(bar, common.CfgDriverFeatureSelect)]
			if sel == 0 {
				d.driverFeatures = (d.driverFeatures &^ 0xFFFFFFFF) | uint64(v)
			} else {
				d.driverFeatures = (d.driverFeatures & 0xFFFFFFFF) | (uint64(v) << 32)
			}
			return nil
		}
	}
	if off >= 0x1000 && off < 0x2000 {
		d.handleNotify(uint16(v))
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeVsockDevice) Write64(bar uint8, off uint64, v uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			d.qdesc[d.currentQueue] = v
			return nil
		case common.CfgQueueDriver:
			d.qdriver[d.currentQueue] = v
			return nil
		case common.CfgQueueDevice:
			d.qdevice[d.currentQueue] = v
			return nil
		}
	}
	d.bar[barKey(bar, off)] = v
	return nil
}

// handleNotify completes a TX descriptor when the tx-queue doorbell
// rings (mirrors the device side of net's TX path). Other queues are
// no-ops; rx delivery is driven explicitly via deliverRaw.
func (d *fakeVsockDevice) handleNotify(qIdx uint16) {
	if !d.txCompletes || qIdx != TxQueueIdx {
		return
	}
	availAddr := d.qdriver[qIdx]
	usedAddr := d.qdevice[qIdx]
	if availAddr == 0 || usedAddr == 0 {
		return
	}
	size := d.qsize[qIdx]
	availSlice := readBufferBytes(uintptr(availAddr), 4+2*int(size))
	if availSlice == nil {
		return
	}
	availIdx := le.Uint16(availSlice[2:4])
	if availIdx == 0 {
		return
	}
	lastSlot := (availIdx - 1) % size
	descIdx := le.Uint16(availSlice[4+lastSlot*2 : 4+lastSlot*2+2])
	usedSlice := readBufferBytes(uintptr(usedAddr), 4+8*int(size))
	if usedSlice == nil {
		return
	}
	usedIdx := le.Uint16(usedSlice[2:4])
	slot := usedIdx % size
	uo := 4 + int(slot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(descIdx))
	le.PutUint32(usedSlice[uo+4:uo+8], 0)
	le.PutUint16(usedSlice[2:4], usedIdx+1)
}

// deliverRaw injects raw bytes into the next available rx descriptor and
// posts a used-ring entry reporting reportLen. Returns false if the
// driver has not posted an rx buffer to consume.
func (d *fakeVsockDevice) deliverRaw(raw []byte, reportLen uint32) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	const q = RxQueueIdx
	availAddr := d.qdriver[q]
	usedAddr := d.qdevice[q]
	descAddr := d.qdesc[q]
	if availAddr == 0 || usedAddr == 0 || descAddr == 0 {
		return false
	}
	size := d.qsize[q]
	availSlice := readBufferBytes(uintptr(availAddr), 4+2*int(size))
	if availSlice == nil {
		return false
	}
	availIdx := le.Uint16(availSlice[2:4])
	if d.rxConsumed >= availIdx {
		return false
	}
	slot := d.rxConsumed % size
	descIdx := le.Uint16(availSlice[4+slot*2 : 4+slot*2+2])
	descSlice := readBufferBytes(uintptr(descAddr), 16*int(size))
	o := int(descIdx) * 16
	bufAddr := le.Uint64(descSlice[o : o+8])
	bufLen := le.Uint32(descSlice[o+8 : o+12])
	n := len(raw)
	if uint32(n) > bufLen {
		n = int(bufLen)
	}
	copy(readBufferBytes(uintptr(bufAddr), n), raw[:n])
	usedSlice := readBufferBytes(uintptr(usedAddr), 4+8*int(size))
	usedIdx := le.Uint16(usedSlice[2:4])
	uslot := usedIdx % size
	uo := 4 + int(uslot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(descIdx))
	le.PutUint32(usedSlice[uo+4:uo+8], reportLen)
	le.PutUint16(usedSlice[2:4], usedIdx+1)
	d.rxConsumed++
	return true
}

// deliverPacket marshals p and injects it; reportLen overrides the
// used-ring length when >= 0 (for short/over-length tests).
func (d *fakeVsockDevice) deliverPacket(p Packet, reportLen int) bool {
	raw := make([]byte, VsockHdrSize+len(p.Data))
	marshalHdr(raw[:VsockHdrSize], p, uint32(len(p.Data)))
	copy(raw[VsockHdrSize:], p.Data)
	rl := uint32(len(raw))
	if reportLen >= 0 {
		rl = uint32(reportLen)
	}
	return d.deliverRaw(raw, rl)
}

// buildVirtioVsockCfgSpace: VID 0x1AF4 / DID 0x1053, CommonCfg +
// extended NotifyCfg (multiplier 4) + DeviceCfg (le64 guest_cid).
func buildVirtioVsockCfgSpace() []byte {
	cfg := make([]byte, 256)
	le.PutUint16(cfg[0:], common.PCIVendorID)
	le.PutUint16(cfg[2:], common.PCIDeviceIDModernVsock)
	le.PutUint16(cfg[6:], common.PCIStatusCapabilityList)
	cfg[0x34] = 0x40

	cfg[0x40] = common.PCICapIDVendorSpecific
	cfg[0x41] = 0x50
	cfg[0x42] = 16
	cfg[0x43] = common.PCICapCommonCfg
	le.PutUint32(cfg[0x48:], 0)
	le.PutUint32(cfg[0x4C:], 0x38)

	cfg[0x50] = common.PCICapIDVendorSpecific
	cfg[0x51] = 0x68
	cfg[0x52] = 20
	cfg[0x53] = common.PCICapNotifyCfg
	le.PutUint32(cfg[0x58:], 0x1000)
	le.PutUint32(cfg[0x5C:], 0x100)
	le.PutUint32(cfg[0x60:], 4) // notify_off_multiplier

	cfg[0x68] = common.PCICapIDVendorSpecific
	cfg[0x69] = 0x00
	cfg[0x6A] = 16
	cfg[0x6B] = common.PCICapDeviceCfg
	le.PutUint32(cfg[0x70:], deviceCfgOff) // offset within BAR
	le.PutUint32(cfg[0x74:], 8)            // length: le64 guest_cid

	return cfg
}

// --- happy path + semantics -------------------------------------------

func TestOpenVirtioVsock_Success(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 42)
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	if v.GuestCID != 42 {
		t.Errorf("GuestCID: got %d, want 42", v.GuestCID)
	}
	if v.NegotiatedFeatures != common.FeatureVersion1 {
		t.Errorf("Negotiated: got 0x%x", v.NegotiatedFeatures)
	}
	if v.RxQueue() == nil || v.TxQueue() == nil || v.EventQueue() == nil {
		t.Error("a queue accessor returned nil")
	}
}

func TestAcceptFeatures(t *testing.T) {
	if got, err := AcceptFeatures(common.FeatureVersion1 | (1 << 1)); err != nil || got != common.FeatureVersion1 {
		t.Errorf("modern: got 0x%x, %v", got, err)
	}
	if _, err := AcceptFeatures(1 << 1); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("legacy: got %v", err)
	}
}

func TestOpenVirtioVsock_WrongDeviceID(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	le.PutUint16(d.cfg[2:], common.PCIDeviceIDModernNet)
	if _, err := OpenVirtioVsock(d); !errors.Is(err, ErrInitWrongDeviceID) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioVsock_LegacyDevice(t *testing.T) {
	d := newFakeVsockDevice(1<<1, 3) // no VERSION_1
	if _, err := OpenVirtioVsock(d); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioVsock_FeaturesNotOK(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	d.clearFeaturesOK = true
	if _, err := OpenVirtioVsock(d); !errors.Is(err, ErrFeaturesNotOK) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioVsock_QueueZeroSize(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	d.qsize[0] = 0
	if _, err := OpenVirtioVsock(d); !errors.Is(err, ErrQueueNotAvailable) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioVsock_QueueSizeClampAndRound(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	d.qsize[0] = 6 // clamp 32->6, round 6->4
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	if got := v.RxQueue().Layout.Size; got != 4 {
		t.Errorf("rx size: got %d, want 4", got)
	}
}

// --- packet TX / RX ---------------------------------------------------

func TestSendPacket_RoundTrip(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	err = v.SendPacket(Packet{
		SrcCID: v.GuestCID, DstCID: CIDHost,
		SrcPort: 1024, DstPort: 5000,
		Type: TypeStream, Op: OpRequest,
		Data: []byte("hello"),
	})
	if err != nil {
		t.Errorf("SendPacket: %v", err)
	}
}

func TestSendPacket_Timeout(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	d.txCompletes = false
	if err := v.SendPacket(Packet{Op: OpRW}); !errors.Is(err, ErrTransmitTimeout) {
		t.Errorf("got %v", err)
	}
}

func TestSendPacket_TooLarge(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	big := make([]byte, int(common.PageSize)) // hdr + page > page
	if err := v.SendPacket(Packet{Data: big}); !errors.Is(err, ErrPacketTooLarge) {
		t.Errorf("got %v", err)
	}
}

func TestSendPacket_AllocFail(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	d.allocFail = true
	if err := v.SendPacket(Packet{}); err == nil {
		t.Error("expected alloc error")
	}
}

func TestSendPacket_AllocZeroPhys(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	it := newInject(d, false)
	v, err := OpenVirtioVsock(it)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	it.enable = true
	it.zeroPhys = true
	if err := v.SendPacket(Packet{}); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v", err)
	}
}

func TestSendPacket_NotifyFail(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	it := newInject(d, false)
	v, err := OpenVirtioVsock(it)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	it.enable = true
	it.fp = failPoint{"Write32", 1} // tx doorbell
	if err := v.SendPacket(Packet{}); err == nil {
		t.Error("expected notify error")
	}
}

func TestSendPacket_QueueFull(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	q := v.TxQueue()
	phys, _, _ := d.AllocatePages(1)
	for i := uint16(0); i < q.Layout.Size; i++ {
		if _, err := q.AddBuffer(uintptr(phys), phys, 64, false); err != nil {
			t.Fatalf("pre-fill[%d]: %v", i, err)
		}
	}
	if err := v.SendPacket(Packet{}); err == nil {
		t.Error("expected queue-full error")
	}
}

func TestReceivePacket_RoundTrip(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	want := Packet{
		SrcCID: CIDHost, DstCID: v.GuestCID,
		SrcPort: 5000, DstPort: 1024,
		Type: TypeStream, Op: OpResponse,
		BufAlloc: 65536, FwdCnt: 7,
		Data: []byte("world!"),
	}
	if !d.deliverPacket(want, -1) {
		t.Fatal("deliverPacket: no rx buffer available")
	}
	got, err := v.ReceivePacket(10000)
	if err != nil {
		t.Fatalf("ReceivePacket: %v", err)
	}
	if got.SrcCID != want.SrcCID || got.DstPort != want.DstPort ||
		got.Op != want.Op || got.BufAlloc != want.BufAlloc || got.FwdCnt != want.FwdCnt {
		t.Errorf("header mismatch: got %+v", got)
	}
	if !bytes.Equal(got.Data, want.Data) {
		t.Errorf("payload: got %q, want %q", got.Data, want.Data)
	}
}

func TestReceivePacket_Timeout(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	if _, err := v.ReceivePacket(100); !errors.Is(err, ErrReceiveTimeout) {
		t.Errorf("got %v", err)
	}
}

func TestReceivePacket_ShortPacket(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	if !d.deliverRaw([]byte{1, 2, 3, 4, 5}, 5) { // < VsockHdrSize
		t.Fatal("deliverRaw failed")
	}
	if _, err := v.ReceivePacket(10000); !errors.Is(err, ErrShortPacket) {
		t.Errorf("got %v", err)
	}
}

func TestReceivePacket_OverLengthClamp(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	v, err := OpenVirtioVsock(d)
	if err != nil {
		t.Fatalf("OpenVirtioVsock: %v", err)
	}
	// Header claims 100 bytes of payload but only 20 are delivered.
	p := Packet{Op: OpRW, Data: make([]byte, 20)}
	raw := make([]byte, VsockHdrSize+20)
	marshalHdr(raw[:VsockHdrSize], p, 100) // len field = 100 (a lie)
	if !d.deliverRaw(raw, uint32(len(raw))) {
		t.Fatal("deliverRaw failed")
	}
	got, err := v.ReceivePacket(10000)
	if err != nil {
		t.Fatalf("ReceivePacket: %v", err)
	}
	if len(got.Data) != 20 {
		t.Errorf("clamped payload: got %d bytes, want 20", len(got.Data))
	}
}

// --- white-box fillQueue branches -------------------------------------

func newQueue(t *testing.T, d common.Transport, size uint16) *common.Virtqueue {
	q, err := common.NewVirtqueue(d, size, 0, 0)
	if err != nil {
		t.Fatalf("NewVirtqueue: %v", err)
	}
	return q
}

func TestFillQueue_AllocFail(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	q := newQueue(t, d, 2)
	d.allocFail = true
	v := &VirtioVsock{transport: d}
	if err := v.fillQueue(q); err == nil {
		t.Error("expected alloc error")
	}
}

func TestFillQueue_ZeroPhys(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	q := newQueue(t, d, 2)
	it := newInject(d, true)
	it.zeroPhys = true
	v := &VirtioVsock{transport: it}
	if err := v.fillQueue(q); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v", err)
	}
}

func TestFillQueue_QueueFull(t *testing.T) {
	d := newFakeVsockDevice(common.FeatureVersion1, 3)
	q := newQueue(t, d, 1)
	phys, _, _ := d.AllocatePages(1)
	if _, err := q.AddBuffer(uintptr(phys), phys, 64, true); err != nil {
		t.Fatalf("saturate: %v", err)
	}
	v := &VirtioVsock{transport: d}
	if err := v.fillQueue(q); err == nil {
		t.Error("expected queue-full error")
	}
}

func TestSentinelError(t *testing.T) {
	if got := ErrReceiveTimeout.Error(); got != string(ErrReceiveTimeout) {
		t.Errorf("Error(): %q", got)
	}
}

func TestReadBufferBytes_Guard(t *testing.T) {
	if readBufferBytes(0, 4) != nil {
		t.Error("addr=0 should return nil")
	}
	buf := make([]byte, 8)
	if readBufferBytes(uintptrFromSlice(buf), 0) != nil {
		t.Error("length=0 should return nil")
	}
}

// --- injection harness + transport-error coverage ---------------------

var errInjected = errors.New("injected transport failure")

type failPoint struct {
	method string
	nth    int
}

type injectTransport struct {
	*fakeVsockDevice
	fp            failPoint
	counts        map[string]int
	enable        bool
	zeroPhys      bool
	zeroPhysAfter int // only zero allocs strictly after this many alloc calls
	allocCalls    int
}

func newInject(d *fakeVsockDevice, enable bool) *injectTransport {
	return &injectTransport{fakeVsockDevice: d, counts: map[string]int{}, enable: enable}
}

func (t *injectTransport) fail(m string) bool {
	if !t.enable || t.fp.method != m {
		return false
	}
	t.counts[m]++
	return t.counts[m] == t.fp.nth
}

func (t *injectTransport) ReadConfig16(o uint8) (uint16, error) {
	if t.fail("ReadConfig16") {
		return 0, errInjected
	}
	return t.fakeVsockDevice.ReadConfig16(o)
}
func (t *injectTransport) Read8(b uint8, o uint64) (uint8, error) {
	if t.fail("Read8") {
		return 0, errInjected
	}
	return t.fakeVsockDevice.Read8(b, o)
}
func (t *injectTransport) Read16(b uint8, o uint64) (uint16, error) {
	if t.fail("Read16") {
		return 0, errInjected
	}
	return t.fakeVsockDevice.Read16(b, o)
}
func (t *injectTransport) Read64(b uint8, o uint64) (uint64, error) {
	if t.fail("Read64") {
		return 0, errInjected
	}
	return t.fakeVsockDevice.Read64(b, o)
}
func (t *injectTransport) Write8(b uint8, o uint64, v uint8) error {
	if t.fail("Write8") {
		return errInjected
	}
	return t.fakeVsockDevice.Write8(b, o, v)
}
func (t *injectTransport) Write16(b uint8, o uint64, v uint16) error {
	if t.fail("Write16") {
		return errInjected
	}
	return t.fakeVsockDevice.Write16(b, o, v)
}
func (t *injectTransport) Write32(b uint8, o uint64, v uint32) error {
	if t.fail("Write32") {
		return errInjected
	}
	return t.fakeVsockDevice.Write32(b, o, v)
}
func (t *injectTransport) Write64(b uint8, o uint64, v uint64) error {
	if t.fail("Write64") {
		return errInjected
	}
	return t.fakeVsockDevice.Write64(b, o, v)
}
func (t *injectTransport) AllocatePages(c int) (uint64, []byte, error) {
	if t.fail("AllocatePages") {
		return 0, nil, errInjected
	}
	t.allocCalls++
	phys, mem, err := t.fakeVsockDevice.AllocatePages(c)
	if t.enable && t.zeroPhys && t.allocCalls > t.zeroPhysAfter {
		return 0, mem, nil
	}
	return phys, mem, err
}

// TestOpenVirtioVsock_TransportErrors drives every `if err != nil` return
// in OpenVirtioVsock + setupQueue (at the rx-queue invocation) by failing
// the corresponding transport call. Counts follow the fixed bring-up
// order; queue-register sites are hit on the first (rx) setupQueue call.
func TestOpenVirtioVsock_TransportErrors(t *testing.T) {
	cases := []struct {
		name string
		fp   failPoint
	}{
		{"DIDRead", failPoint{"ReadConfig16", 1}},
		{"InitModernConfig", failPoint{"ReadConfig16", 2}},
		{"ResetStatus", failPoint{"Write8", 1}},
		{"PostResetStatusRead", failPoint{"Read8", 1}},
		{"AckStatus", failPoint{"Write8", 2}},
		{"DriverStatus", failPoint{"Write8", 3}},
		{"DeviceFeatures", failPoint{"Write32", 1}},
		{"DriverFeatures", failPoint{"Write32", 3}},
		{"FeaturesOKStatus", failPoint{"Write8", 4}},
		{"PostFeaturesStatusRead", failPoint{"Read8", 2}},
		{"SelectQueue", failPoint{"Write16", 1}},
		{"QueueSize", failPoint{"Read16", 1}},
		{"SetQueueSize", failPoint{"Write16", 2}},
		{"QueueNotifyOff", failPoint{"Read16", 2}},
		{"AllocVirtqueue", failPoint{"AllocatePages", 1}},
		{"SetQueueDesc", failPoint{"Write64", 1}},
		{"SetQueueDriver", failPoint{"Write64", 2}},
		{"SetQueueDevice", failPoint{"Write64", 3}},
		{"SetQueueEnable", failPoint{"Write16", 3}},
		{"TxQueueSetup", failPoint{"Write16", 4}},    // SelectQueue on the 2nd (tx) queue
		{"EventQueueSetup", failPoint{"Write16", 7}}, // SelectQueue on the 3rd (event) queue
		{"DriverOKStatus", failPoint{"Write8", 5}},
		{"GuestCIDRead", failPoint{"Read64", 1}},
		{"RxNotify", failPoint{"Write32", 7}}, // after DeviceFeatures(2)+DriverFeatures(4)
		{"EventNotify", failPoint{"Write32", 8}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeVsockDevice(common.FeatureVersion1, 3)
			it := newInject(d, true)
			it.fp = tc.fp
			if _, err := OpenVirtioVsock(it); err == nil {
				t.Fatalf("%s: expected error at %+v", tc.name, tc.fp)
			}
		})
	}
}

// TestOpenVirtioVsock_FillErrors covers the two `v.fillQueue(...)` error
// returns in Open. A small (size-1) ring makes the per-queue allocation
// order predictable: #1-3 = the three queue rings, #4 = rx fill, #5 =
// event fill.
func TestOpenVirtioVsock_FillErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		nth  int
	}{
		{"RxFill", 4},
		{"EventFill", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeVsockDevice(common.FeatureVersion1, 3)
			d.qsize = map[uint16]uint16{0: 1, 1: 1, 2: 1}
			it := newInject(d, true)
			it.fp = failPoint{"AllocatePages", tc.nth}
			if _, err := OpenVirtioVsock(it); err == nil {
				t.Fatalf("%s: expected fill alloc error", tc.name)
			}
		})
	}
}
