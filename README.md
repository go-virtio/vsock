<p align="center"><img src="https://raw.githubusercontent.com/go-virtio/brand/main/social/go-virtio-vsock.png" alt="go-virtio/vsock" width="720"></p>

# go-virtio/vsock

Pure-Go virtio-vsock (socket device) driver targeting the
`go-virtio/common` transport interfaces. Implements the modern-transport
(Virtio 1.0+) init sequence and the three-virtqueue packet path for the
standard PCI-bound virtio-vsock device (VID 0x1AF4, DID 0x1053).

## Scope

This package sits at the same altitude as
[`go-virtio/net`](https://github.com/go-virtio/net): it owns device
bring-up, the three virtqueues (rx / tx / event, Virtio 1.1 §5.10.2), and
the on-the-wire `struct virtio_vsock_hdr` marshalling, exposing a
**packet-level** `SendPacket` / `ReceivePacket` API.

It deliberately does **not** implement the connection state machine or
the credit-based flow control (`buf_alloc` / `fwd_cnt` accounting) —
those belong a layer up, exactly as `net` drives frames rather than TCP.
The header's addressing and credit fields are surfaced on `Packet` so the
upper layer can implement them.

## Quick start

```go
import (
    virtiovsock "github.com/go-virtio/vsock"
)

// transport is any value that implements go-virtio/common.Transport.
vs, err := virtiovsock.OpenVirtioVsock(transport)
if err != nil {
    return err
}

// Connection-request packet to the host (CID 2), port 1024.
err = vs.SendPacket(virtiovsock.Packet{
    SrcCID:  vs.GuestCID,
    DstCID:  virtiovsock.CIDHost,
    SrcPort: 1024,
    DstPort: 5000,
    Type:    virtiovsock.TypeStream,
    Op:      virtiovsock.OpRequest,
})

// Poll for one inbound packet.
pkt, err := vs.ReceivePacket(10000) // busy-poll budget
```

`OpenVirtioVsock` leaves the device in DRIVER_OK state with the rx and
event queues pre-posted and `GuestCID` populated from device config.

## Sibling packages

  - [`github.com/go-virtio/common`](https://github.com/go-virtio/common)
    — transport-agnostic infrastructure (PCI cap walker, modern config
    layout, split-virtqueue impl, transport interfaces).
  - [`github.com/go-virtio/net`](https://github.com/go-virtio/net) —
    pure-Go virtio-net driver (the reference per-device-class driver this
    package mirrors).
  - [`github.com/go-virtio/rng`](https://github.com/go-virtio/rng) —
    pure-Go virtio-rng driver.
  - [`github.com/go-virtio/blk`](https://github.com/go-virtio/blk) —
    placeholder for a future pure-Go virtio-blk driver.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
