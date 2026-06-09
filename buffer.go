// Buffer helper — turns a uintptr buffer address (returned by the
// PageAllocator and stored in the virtqueue's per-descriptor
// bookkeeping) back into a Go byte slice. Lives in its own file so the
// `unsafe` import is contained.

package vsock

import "unsafe"

// readBufferBytes returns a Go byte view of `length` bytes starting at
// host-virtual address `addr`. On identity-mapped UEFI hosts this equals
// the physical address; on hosts with a separate kernel-virtual mapping
// the PageAllocator implementation has already translated.
//
// The returned slice aliases the underlying DMA buffer — ReceivePacket
// copies bytes out before the descriptor is reused, so callers never see
// the aliasing.
func readBufferBytes(addr uintptr, length int) []byte {
	if addr == 0 || length <= 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), length)
}
