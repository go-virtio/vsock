# Performance — go-virtio virtio-vsock driver hot-path efficiency (2026-06-22)

This measures the **virtio-vsock per-packet header path** — the controllable
guest-side overhead the driver adds per packet: the 44-byte
`virtio_vsock_hdr` marshal (TX) and parse (RX).

The ring management (descriptor/avail/used) lives in `go-virtio/common`;
see its `BENCHMARKS.md` for the virtqueue hot-path numbers and the honest
discussion of why an end-to-end throughput comparison against the **Linux
kernel** vhost-vsock driver is **not apples-to-apples** (end-to-end vsock
throughput is set by the host vhost backend, not the guest header code).

## Methodology

- **CPU:** Apple M4 Max (16 logical CPUs). **OS:** macOS 26.5. **Go:** 1.26.4
  (`darwin/arm64`). **CGO_ENABLED=0**, `GOWORK=off`.
- **Isolated-micro:** header marshal/parse only — no device, no DMA, no VMM.
- Best-of-3 (`-count=3`), `-benchmem`. Values are the median.
- Reproduce:
  `GOWORK=off CGO_ENABLED=0 go test -run '^$' -bench . -benchmem -count=3 ./...`
- Benchmarks live in `bench_test.go` so they do **not** affect the 100%
  coverage gate (they run only under `-bench`).

## Results (isolated-micro — our per-packet controllable overhead)

| path | ns/op | throughput | allocs/op | note |
|------|------:|-----------:|----------:|------|
| `marshalHdr` (TX, into caller buffer) | 0.26 | — | 0 | **zero-copy, zero-alloc** — the ideal shape |
| `parsePacket` header-only (control op) | 4.5 | — | 0 | decode 44-byte header, empty payload |
| `parsePacket` 1500 B payload (RX) | 181 | ~8.3 GB/s | 1 | header decode + defensive payload copy |

## Summary

- **TX is optimal: `marshalHdr` writes the 44-byte header directly into a
  caller-owned buffer — zero allocations, ~0.26 ns.** This is the pattern the
  virtio-net TX path (`go-virtio/net`) should adopt to drop its per-frame
  allocation.
- **RX costs one allocation per packet** — `parsePacket` copies the payload
  out so the caller may retain it after the descriptor is reclaimed and
  re-posted. This defensive copy is deliberate (correctness over zero-copy);
  at ~8.3 GB/s it is well below the device round-trip cost.

### Action items (our controllable overhead)

1. **Optional borrow-mode RX.** Offer a `parsePacketInto`/borrowed-slice
   variant for callers that consume the payload before re-posting the
   descriptor, eliminating the per-packet copy where the caller can promise
   not to retain. Keep the copying `parsePacket` as the safe default.
