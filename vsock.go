// Package vsock is a pure-Go virtio-vsock (socket device) driver. It
// drives a modern (Virtio 1.0+) PCI virtio-vsock device through the
// transport interfaces defined in github.com/go-virtio/common; the same
// code drives a UEFI-backed device, a bare-metal device, or a
// virtio-mmio device depending on which common.Transport implementation
// the caller supplies.
//
// Scope — this package sits at the same altitude as go-virtio/net: it
// owns the device bring-up, the three virtqueues, and the on-the-wire
// struct virtio_vsock_hdr marshalling, and exposes a packet-level
// Send/Receive API. It deliberately does NOT implement the connection
// state machine or the credit-based flow control (buf_alloc / fwd_cnt
// accounting) — those belong a layer up, just as net drives frames, not
// TCP. The header's addressing and credit fields are surfaced on Packet
// so that upper layer can implement them.
//
//   - Modern transport (VIRTIO_F_VERSION_1 mandatory). Legacy devices
//     are rejected by the common init sequence.
//   - Split-virtqueue layout; the packed ring is negotiated OUT.
//   - Three virtqueues: rx (0), tx (1), event (2) per Virtio 1.1 §5.10.2.
//
// References:
//
//   - Virtio 1.1 §5.10   "Socket Device" — device-type 19 binding.
//   - Virtio 1.1 §5.10.2 "Virtqueues" — rx / tx / event.
//   - Virtio 1.1 §5.10.4 "Device configuration layout" — le64 guest_cid.
//   - Virtio 1.1 §5.10.6 "Device Operation" — struct virtio_vsock_hdr.
//   - Virtio 1.1 §3.1.1  "Device Initialization" — the status-bit
//     choreography in OpenVirtioVsock.
package vsock

import (
	"encoding/binary"

	"github.com/go-virtio/common"
)

// Virtqueue indices (Virtio 1.1 §5.10.2).
const (
	RxQueueIdx    uint16 = 0
	TxQueueIdx    uint16 = 1
	EventQueueIdx uint16 = 2
)

// Desired ring sizes (clamped down to the device maximum, rounded to a
// power of two, during setup). The event queue is tiny — events are
// rare (only transport resets in this scope).
const (
	RxRingSize    uint16 = 32
	TxRingSize    uint16 = 32
	EventRingSize uint16 = 4
)

// VsockHdrSize is the on-the-wire byte length of struct virtio_vsock_hdr
// (Virtio 1.1 §5.10.6.1), all fields little-endian:
//
//	0   le64 src_cid
//	8   le64 dst_cid
//	16  le32 src_port
//	20  le32 dst_port
//	24  le32 len          (payload byte count following the header)
//	28  le16 type
//	30  le16 op
//	32  le32 flags
//	36  le32 buf_alloc
//	40  le32 fwd_cnt
const VsockHdrSize = 44

// Packet type values (Virtio 1.1 §5.10.6 — virtio_vsock_hdr.type).
const (
	TypeStream    uint16 = 1
	TypeSeqpacket uint16 = 2
)

// Operation codes (Virtio 1.1 §5.10.6 — virtio_vsock_hdr.op).
const (
	OpInvalid       uint16 = 0
	OpRequest       uint16 = 1
	OpResponse      uint16 = 2
	OpRst           uint16 = 3
	OpShutdown      uint16 = 4
	OpRW            uint16 = 5
	OpCreditUpdate  uint16 = 6
	OpCreditRequest uint16 = 7
)

// Well-known context IDs (Virtio 1.1 §5.10.4). VMADDR_CID_HOST = 2 is
// the peer CID a guest uses to reach the host.
const (
	CIDHypervisor uint64 = 0
	CIDHost       uint64 = 2
	CIDAny        uint64 = 0xFFFFFFFF
)

// TxPollIterations is the default busy-poll budget for SendPacket while
// waiting for the device to return the transmitted descriptor.
const TxPollIterations = 200000

// Packet is one virtio-vsock packet: the header fields plus the payload.
// On SendPacket the `len` header field is derived from len(Data); on
// ReceivePacket Data is the payload the device delivered.
type Packet struct {
	SrcCID, DstCID   uint64
	SrcPort, DstPort uint32
	Type             uint16
	Op               uint16
	Flags            uint32
	BufAlloc         uint32
	FwdCnt           uint32
	Data             []byte
}

// marshalHdr writes p's header fields into dst[:VsockHdrSize], using
// payloadLen for the `len` field. dst MUST be at least VsockHdrSize long.
func marshalHdr(dst []byte, p Packet, payloadLen uint32) {
	binary.LittleEndian.PutUint64(dst[0:], p.SrcCID)
	binary.LittleEndian.PutUint64(dst[8:], p.DstCID)
	binary.LittleEndian.PutUint32(dst[16:], p.SrcPort)
	binary.LittleEndian.PutUint32(dst[20:], p.DstPort)
	binary.LittleEndian.PutUint32(dst[24:], payloadLen)
	binary.LittleEndian.PutUint16(dst[28:], p.Type)
	binary.LittleEndian.PutUint16(dst[30:], p.Op)
	binary.LittleEndian.PutUint32(dst[32:], p.Flags)
	binary.LittleEndian.PutUint32(dst[36:], p.BufAlloc)
	binary.LittleEndian.PutUint32(dst[40:], p.FwdCnt)
}

// parsePacket decodes a received buffer (header + payload) into a
// Packet. `raw` is the device-reported byte view (length = used-ring
// len). The payload is copied so the caller may retain it after the
// descriptor is reclaimed and re-posted.
func parsePacket(raw []byte) (Packet, error) {
	if len(raw) < VsockHdrSize {
		return Packet{}, ErrShortPacket
	}
	payloadLen := binary.LittleEndian.Uint32(raw[24:])
	end := VsockHdrSize + int(payloadLen)
	if end > len(raw) {
		// Device reported more payload than it delivered; clamp to what
		// is actually present rather than read out of bounds.
		end = len(raw)
	}
	data := make([]byte, end-VsockHdrSize)
	copy(data, raw[VsockHdrSize:end])
	return Packet{
		SrcCID:   binary.LittleEndian.Uint64(raw[0:]),
		DstCID:   binary.LittleEndian.Uint64(raw[8:]),
		SrcPort:  binary.LittleEndian.Uint32(raw[16:]),
		DstPort:  binary.LittleEndian.Uint32(raw[20:]),
		Type:     binary.LittleEndian.Uint16(raw[28:]),
		Op:       binary.LittleEndian.Uint16(raw[30:]),
		Flags:    binary.LittleEndian.Uint32(raw[32:]),
		BufAlloc: binary.LittleEndian.Uint32(raw[36:]),
		FwdCnt:   binary.LittleEndian.Uint32(raw[40:]),
		Data:     data,
	}, nil
}

// AcceptedFeatures is the feature mask the driver negotiates ON. The
// packet-level driver needs no vsock-specific feature bit (stream is the
// baseline); the only bit we accept is the non-negotiable
// VIRTIO_F_VERSION_1.
const AcceptedFeatures uint64 = common.FeatureVersion1

// AcceptFeatures returns the negotiated feature mask: the intersection
// of what the device offers and what we accept. Requires
// VIRTIO_F_VERSION_1 (else the device is legacy-only).
func AcceptFeatures(deviceFeatures uint64) (uint64, error) {
	if deviceFeatures&common.FeatureVersion1 == 0 {
		return 0, ErrNotModernDevice
	}
	return deviceFeatures & AcceptedFeatures, nil
}

// VirtioVsock wraps one initialised virtio-vsock device.
type VirtioVsock struct {
	// Cfg is the modern-transport handle.
	Cfg *common.ModernConfig

	// GuestCID is the context ID the device assigned to this guest
	// (Virtio 1.1 §5.10.4), read from DeviceCfg at OpenVirtioVsock.
	GuestCID uint64

	// NegotiatedFeatures records the driver-feature handshake result.
	NegotiatedFeatures uint64

	transport common.Transport

	// The three virtqueues (Virtio 1.1 §5.10.2).
	rxq    *common.Virtqueue
	txq    *common.Virtqueue
	eventq *common.Virtqueue
}

// OpenVirtioVsock drives the full bring-up of one virtio-vsock device:
//
//  1. Verify the PCI device ID is 0x1053 (modern vsock).
//  2. InitModernConfig walks PCI caps + populates the BAR locators.
//  3. Reset → ACK → DRIVER status progression.
//  4. Read DeviceFeature, mask to VERSION_1, write DriverFeature.
//  5. Set FEATURES_OK, verify it stuck.
//  6. Allocate + publish rx (0), tx (1), event (2) queues.
//  7. DRIVER_OK status.
//  8. Read guest_cid from DeviceCfg.
//  9. Pre-post receive + event buffers and notify the device.
func OpenVirtioVsock(t common.Transport) (*VirtioVsock, error) {
	did, err := t.ReadConfig16(common.PCICfgDeviceID)
	if err != nil {
		return nil, err
	}
	if did != common.PCIDeviceIDModernVsock {
		return nil, ErrInitWrongDeviceID
	}

	cfg, err := common.InitModernConfig(t)
	if err != nil {
		return nil, err
	}

	// Step 1: full reset.
	if err := cfg.SetDeviceStatus(0); err != nil {
		return nil, err
	}
	if _, err := cfg.DeviceStatus(); err != nil {
		return nil, err
	}

	// Steps 2–3: ACKNOWLEDGE, DRIVER.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge); err != nil {
		return nil, err
	}
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver); err != nil {
		return nil, err
	}

	// Step 4: feature negotiation.
	deviceFeats, err := cfg.DeviceFeatures64()
	if err != nil {
		return nil, err
	}
	if deviceFeats&common.FeatureVersion1 == 0 {
		return nil, ErrNotModernDevice
	}
	negotiated := deviceFeats & AcceptedFeatures
	if err := cfg.SetDriverFeatures64(negotiated); err != nil {
		return nil, err
	}

	// Step 5: FEATURES_OK + verify.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK); err != nil {
		return nil, err
	}
	status, err := cfg.DeviceStatus()
	if err != nil {
		return nil, err
	}
	if status&common.StatusFeaturesOK == 0 {
		return nil, ErrFeaturesNotOK
	}

	// Step 6: queue setup.
	rxq, err := setupQueue(cfg, t, RxQueueIdx, RxRingSize)
	if err != nil {
		return nil, err
	}
	txq, err := setupQueue(cfg, t, TxQueueIdx, TxRingSize)
	if err != nil {
		return nil, err
	}
	eventq, err := setupQueue(cfg, t, EventQueueIdx, EventRingSize)
	if err != nil {
		return nil, err
	}

	// Step 7: DRIVER_OK.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK | common.StatusDriverOK); err != nil {
		return nil, err
	}

	// Step 8: read guest_cid (le64 at DeviceCfg offset 0).
	cid, err := cfg.DeviceCfgRead64(0)
	if err != nil {
		return nil, err
	}

	v := &VirtioVsock{
		Cfg:                cfg,
		GuestCID:           cid,
		NegotiatedFeatures: negotiated,
		transport:          t,
		rxq:                rxq,
		txq:                txq,
		eventq:             eventq,
	}

	// Step 9: pre-post rx + event buffers so the device has somewhere to
	// land incoming packets and events.
	if err := v.fillQueue(rxq); err != nil {
		return nil, err
	}
	if err := v.fillQueue(eventq); err != nil {
		return nil, err
	}
	if err := cfg.NotifyQueue(RxQueueIdx, rxq.NotifyOff); err != nil {
		return nil, err
	}
	if err := cfg.NotifyQueue(EventQueueIdx, eventq.NotifyOff); err != nil {
		return nil, err
	}

	return v, nil
}

// setupQueue performs the per-queue init: select, read max-size, write
// our size (= min(desired, max), rounded down to a power of two),
// allocate the Virtqueue, publish its addresses, enable.
func setupQueue(cfg *common.ModernConfig, t common.Transport, queueIdx uint16, desiredSize uint16) (*common.Virtqueue, error) {
	if err := cfg.SelectQueue(queueIdx); err != nil {
		return nil, err
	}
	maxSize, err := cfg.QueueSize()
	if err != nil {
		return nil, err
	}
	if maxSize == 0 {
		// maxSize >= 1 from here on, so the size computed below is always
		// a non-zero power of two — no further zero-check needed.
		return nil, ErrQueueNotAvailable
	}
	size := desiredSize
	if size > maxSize {
		size = maxSize
	}
	for size&(size-1) != 0 {
		size &= size - 1
	}
	if err := cfg.SetQueueSize(size); err != nil {
		return nil, err
	}
	notifyOff, err := cfg.QueueNotifyOff()
	if err != nil {
		return nil, err
	}
	q, err := common.NewVirtqueue(t, size, queueIdx, notifyOff)
	if err != nil {
		return nil, err
	}
	descAddr := q.BasePhys + uint64(q.Layout.DescTableOffset)
	availAddr := q.BasePhys + uint64(q.Layout.AvailRingOffset)
	usedAddr := q.BasePhys + uint64(q.Layout.UsedRingOffset)
	if err := cfg.SetQueueDesc(descAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDriver(availAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDevice(usedAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueEnable(1); err != nil {
		return nil, err
	}
	return q, nil
}

// fillQueue posts one page-sized device-writable buffer per descriptor
// slot, giving the device landing zones for incoming packets/events.
func (v *VirtioVsock) fillQueue(q *common.Virtqueue) error {
	for i := uint16(0); i < q.Layout.Size; i++ {
		phys, mem, err := v.transport.AllocatePages(1)
		if err != nil {
			return err
		}
		if phys == 0 {
			return common.ErrAllocReturnedZero
		}
		if _, err := q.AddBuffer(uintptr(phys), phys, uint32(len(mem)), true); err != nil {
			return err
		}
	}
	return nil
}

// RxQueue / TxQueue / EventQueue expose the per-direction virtqueue
// handles for diagnostic inspection.
func (v *VirtioVsock) RxQueue() *common.Virtqueue    { return v.rxq }
func (v *VirtioVsock) TxQueue() *common.Virtqueue    { return v.txq }
func (v *VirtioVsock) EventQueue() *common.Virtqueue { return v.eventq }

// SendPacket marshals p (header + payload) into a fresh DMA buffer,
// enqueues it on the tx queue, notifies the device, and busy-polls the
// used ring for completion. Returns ErrPacketTooLarge if the payload
// plus header exceeds one page, or ErrTransmitTimeout if the device does
// not return the descriptor within TxPollIterations.
func (v *VirtioVsock) SendPacket(p Packet) error {
	total := VsockHdrSize + len(p.Data)
	phys, mem, err := v.transport.AllocatePages(1)
	if err != nil {
		return err
	}
	if phys == 0 {
		return common.ErrAllocReturnedZero
	}
	if total > len(mem) {
		return ErrPacketTooLarge
	}
	marshalHdr(mem[:VsockHdrSize], p, uint32(len(p.Data)))
	copy(mem[VsockHdrSize:], p.Data)

	if _, err := v.txq.AddBuffer(uintptr(phys), phys, uint32(total), false); err != nil {
		return err
	}
	if err := v.Cfg.NotifyQueue(TxQueueIdx, v.txq.NotifyOff); err != nil {
		return err
	}
	for spin := 0; spin < TxPollIterations; spin++ {
		gotIdx, _, ok := v.txq.PollUsed()
		if !ok {
			continue
		}
		_ = v.txq.Reclaim(gotIdx)
		return nil
	}
	return ErrTransmitTimeout
}

// ReceivePacket polls the rx queue for one packet, busy-spinning up to
// pollIterations cycles. On success it copies the payload out, reclaims
// and re-posts the descriptor (best-effort), and returns the parsed
// Packet. Returns ErrReceiveTimeout if no packet arrives in budget.
func (v *VirtioVsock) ReceivePacket(pollIterations int) (Packet, error) {
	for spin := 0; spin < pollIterations; spin++ {
		descIdx, length, ok := v.rxq.PollUsed()
		if !ok {
			continue
		}
		buf := v.rxq.Buffers[descIdx]
		raw := readBufferBytes(buf.Addr, int(length))
		out := make([]byte, len(raw))
		copy(out, raw)
		_ = v.rxq.Reclaim(descIdx)
		// Best-effort re-post + doorbell; a failure here only degrades
		// future throughput, the captured packet is still valid.
		_, _ = v.rxq.AddBuffer(buf.Addr, buf.Phys, buf.Len, true)
		_ = v.Cfg.NotifyQueue(RxQueueIdx, v.rxq.NotifyOff)
		return parsePacket(out)
	}
	return Packet{}, ErrReceiveTimeout
}

// Sentinel errors for the virtio-vsock path.
var (
	ErrNotModernDevice   = commonVsockError("go-virtio/vsock: device doesn't offer VIRTIO_F_VERSION_1 (legacy-only)")
	ErrFeaturesNotOK     = commonVsockError("go-virtio/vsock: FEATURES_OK status bit didn't stick after DriverFeature write")
	ErrInitWrongDeviceID = commonVsockError("go-virtio/vsock: PCI device ID is not 0x1053 (modern vsock device)")
	ErrQueueNotAvailable = commonVsockError("go-virtio/vsock: device reports QueueSize=0 for a required queue")
	ErrTransmitTimeout   = commonVsockError("go-virtio/vsock: TX poll timeout (device did not return descriptor)")
	ErrReceiveTimeout    = commonVsockError("go-virtio/vsock: RX poll timeout (no packet received within budget)")
	ErrPacketTooLarge    = commonVsockError("go-virtio/vsock: header + payload exceeds one page")
	ErrShortPacket       = commonVsockError("go-virtio/vsock: received buffer shorter than virtio_vsock_hdr (44 bytes)")
)

// commonVsockError is the package's tiny sentinel-error type — same
// pattern as go-virtio/common.commonError and go-virtio/net.commonNetError.
type commonVsockError string

func (e commonVsockError) Error() string { return string(e) }
