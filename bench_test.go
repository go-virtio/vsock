// Driver hot-path micro-benchmarks for the virtio-vsock per-packet header
// path. These measure ONLY go-virtio's controllable overhead (the 44-byte
// virtio_vsock_hdr marshal/parse) — no real device, no host VMM.
// End-to-end vsock throughput is dominated by the device + hypervisor; the
// ring management lives in go-virtio/common (see its BENCHMARKS.md for the
// virtqueue hot paths and the honest kernel-comparison framing).
//
// Run:  GOWORK=off go test -run x -bench . -benchmem ./...
//
// Benchmarks live in a _test.go file so they do NOT affect the
// statement-coverage gate (they execute only under -bench).

package vsock

import "testing"

var benchPkt = Packet{
	SrcCID:   3,
	DstCID:   2,
	SrcPort:  1024,
	DstPort:  5000,
	Type:     1, // STREAM
	Op:       5, // RW
	Flags:    0,
	BufAlloc: 262144,
	FwdCnt:   0,
}

// BenchmarkMarshalHdr measures the per-packet TX header build. It writes
// into a caller-owned buffer — the zero-alloc pattern. This is the cheap,
// correct shape.
func BenchmarkMarshalHdr(b *testing.B) {
	dst := make([]byte, VsockHdrSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		marshalHdr(dst, benchPkt, 4096)
	}
}

// BenchmarkParsePacket1500 measures the per-packet RX path: header decode
// plus the defensive payload copy (so the caller may retain the payload
// after the descriptor is reclaimed). The copy is one allocation per
// packet — the controllable cost on RX.
func BenchmarkParsePacket1500(b *testing.B) {
	raw := make([]byte, VsockHdrSize+1500)
	// Set the le32 len field (offset 24) to 1500 so parsePacket copies a
	// full payload.
	const plen = 1500
	raw[24] = byte(plen & 0xFF)
	raw[25] = byte((plen >> 8) & 0xFF)
	b.ReportAllocs()
	b.SetBytes(1500)
	b.ResetTimer()
	var sink Packet
	for i := 0; i < b.N; i++ {
		p, err := parsePacket(raw)
		if err != nil {
			b.Fatal(err)
		}
		sink = p
	}
	_ = sink
}

// BenchmarkParsePacketHdrOnly measures parsing a zero-payload control
// packet (CREDIT_UPDATE / connection ops) — header decode with an empty
// payload copy.
func BenchmarkParsePacketHdrOnly(b *testing.B) {
	raw := make([]byte, VsockHdrSize)
	b.ReportAllocs()
	b.ResetTimer()
	var sink Packet
	for i := 0; i < b.N; i++ {
		p, err := parsePacket(raw)
		if err != nil {
			b.Fatal(err)
		}
		sink = p
	}
	_ = sink
}
