# icmptun

**A TCP tunnel carried entirely inside ICMP echo — multiplexed, FEC-protected, and fast
enough to be usable: 1.3 MB/s measured over a 170 ms path.**

Some links let exactly one thing through: plain ICMP echo request/reply. No TCP handshake,
no UDP association, nothing else survives. `icmptun` puts a real TCP proxy inside that one
remaining channel and then does the hard part — making it behave like a link:

- a **60-byte frame** with sequence numbers, a receive window and a **256-bit SACK bitmap**;
- **XOR FEC**, one parity packet per K data packets, so single losses don't cost a round trip;
- an **ICMP id/seq mirroring** trick that survives CGNAT;
- a **progress-based watchdog** that tells "the link is idle" apart from "the tunnel died silently".

Written in Go, **standard library only**, ~1.6k lines. Builds as a single static binary for
Linux, macOS and Windows.

```
  app ──SOCKS5 127.0.0.1:1080──┐                       ┌── target host
  app ──HTTP   127.0.0.1:8118──┤ mux ── ICMP echo ──── ┤
                               └── client ─────────────── server ── net.Dial
```

---

## What this is not

| It is | It is not |
|---|---|
| a TCP proxy over ICMP, for links where TCP/UDP is filtered | a VPN — no tun/tap, no IP forwarding, TCP only |
| a benchmarkable protocol implementation | a stealth or undetectable channel; it is a bare ICMP tunnel |
| a single-purpose tool you can read in an afternoon | production infrastructure; there is no HA, no failover |

## Install

**Prebuilt binaries** for Linux, macOS and Windows (amd64 and arm64) are on the
[releases page](https://github.com/cyrainfall/icmptun/releases), together with a
`SHA256SUMS` file. There is nothing to configure — a single static binary, no
runtime dependencies.

**From source** (Go 1.22+):

```sh
go build -o icmptun .
```

## Verify it without a network

```sh
$ ./icmptun -mode selftest
icmptun selftest: 8 checks, no raw socket needed

  ok    1/8  frame round-trip preserves every field
  ok    2/8  header layout: 60 B, magic/version/linkid/len
  ok    3/8  decoder rejects malformed frames
  ok    4/8  FEC: parity emit + single-loss recovery
  ok    5/8  SACK bitmap: bit set means missing
  ok    6/8  mux framing: [type][stream][len][data]
  ok    7/8  ICMP checksum, valid and suppressed
  ok    8/8  server mirrors the client's ICMP id/seq

8 passed, 0 failed in 412µs
```

No root, no socket, nothing leaves the machine. The checks cover the frame layout,
the FEC parity path (including recovery of a dropped packet), SACK bitmap polarity,
mux framing and the ICMP checksum trick. `go test ./...` runs the same code.

## 30-second start

Two hosts, both with raw-socket privileges (root / Administrator).

```sh
go build -o icmptun .                     # no dependencies, no codegen

# server — on the machine that can reach the internet normally
sudo ./icmptun -mode server -payload 1350 -wnd 4096 -fec 8 -pps 20000 -v

# client — on the restricted machine (203.0.113.10 = your server)
sudo ./icmptun -mode client -peer 203.0.113.10 \
  -socks 127.0.0.1:1080 -http 127.0.0.1:8118 \
  -payload 1350 -wnd 4096 -fec 8 -pps 20000 -suppress=false -v

# verify
curl -x socks5h://127.0.0.1:1080 -o /dev/null \
  -w '%{speed_download} B/s\n' https://cachefly.cachefly.net/10mb.test
```

`-payload` and `-fec` **must match on both ends**. If the tunnel sends frames but nothing
comes back, try `-suppress=false` first — see [Troubleshooting](#troubleshooting).

Requirements: the server needs a listening raw ICMP socket; the client needs the same plus
outbound ICMP to the server. On Windows the process must run elevated, because
`net.ListenIP` builds a raw socket.

## How it works

### One frame, 60 bytes

Every ICMP echo payload is a self-describing frame. Big-endian, fixed layout:

| offset | size | field | notes |
|---:|---:|---|---|
| 0 | 4 | magic `DTH1` | `0x44544831` |
| 4 | 1 | version | currently `1` |
| 5 | 1 | flags | `1` ack-only, `2` parity, `4` ping, `16` from-client |
| 6 | 4 | session | server adopts a new session on first sight |
| 10 | 4 | seq | per-frame sequence |
| 14 | 4 | cumulative ack | everything below this is received |
| 18 | 2 | window | free receive slots, in packets |
| 20 | 32 | **SACK holes** | 256 bits; **bit set = that seq is missing** |
| 52 | 4 | FEC block | |
| 56 | 1 | parity index | |
| 57 | 2 | payload length | |
| 59 | 1 | **link id** | lets several tunnels share one server host |

Default payload is 1350 B, so a frame is `60 + 1350 = 1410 B`; with IP and ICMP headers that
is 1438 B on the wire — under the common 1450 B MTU.

### The five things that took it from 0 to 1.3 MB/s

1. **Mirror the client's ICMP id/seq.** CGNAT only forwards echo replies that match an
   outbound request's identifier. The server therefore never invents its own — it copies the
   most recent id/seq it saw from the client.
2. **The server adopts a new session.** Otherwise both ends wait for the other to yield.
3. **SACK bitmap semantics.** Bit set means *lost*, not *received*. Inverted, it produces a
   retransmission storm instead of recovery.
4. **Raise the receive buffer.** macOS raw sockets default far too small for a 4096-packet
   window; without `setsockopt` the ring overflows before the window can be used.
5. **64 → 256 bits of SACK.** This is the decisive one, and it is arithmetic, not tuning:

   ```
   RTT 170 ms, target 1.3 MB/s
     in-flight needed = 1.3e6 × 0.17 ≈ 221 KB ≈ 164 frames of 1350 B
     64-bit bitmap  → 64 × 1350 / 0.17 ≈ 508 KB/s   ← exactly the ~500 KB/s ceiling we saw
     256-bit bitmap → ≈ 2.0 MB/s, minus ~11% FEC (K=8) ≈ 1.8; measured 1.3
   ```

   When cumulative ack is stuck behind one lost packet, the width of the bitmap *is* your
   throughput on a high-RTT path.

### Two silent killers

- **`-suppress` writes a deliberately invalid ICMP checksum** so the remote kernel doesn't
  auto-reply to every frame. Some paths drop bad-checksum ICMP outright — the tunnel then
  sends nothing at all. Symptom: client `rxfrm 0/s`, and `tcpdump` on the server sees no
  echo requests. Fix: `-suppress=false`.
- **The watchdog measures real progress, not frame arrival.** It looks at whether `sndUna`
  advanced or `rcvNext` advanced. If it watched "did a frame arrive", a server-side session
  reset would look perfectly healthy — frames still flow, `rxfrm` looks great, `inflight`
  never clears, `acked` never moves — and the tunnel would die without anyone noticing.

## Measured throughput

Profile: `-wnd 4096 -fec 8 -payload 1350 -pps 20000`, RTT ≈ 170 ms.

| Path | Direction | Throughput |
|---|---|---|
| macOS client | download | **1.24 – 1.39 MB/s** |
| macOS client | upload | 2.09 MB/s |
| Windows client (`-suppress=false`) | download, run 1 | 634 KB/s |
| Windows client | download, run 2 | 1.11 MB/s |
| Windows client | upload | 3.25 MB/s |
| TLS relay on the same host pair (comparison) | download | 360 KB/s |
| Bare ICMP, no tunnel (path ceiling) | — | 2.2 – 3.7 MB/s |

That is about 60% of the weaker end of the raw range (1.3 vs 2.2 MB/s) and about a third of
the stronger end; the gap is FEC overhead plus loss, not framing.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-mode` | `client` | `client` or `server` |
| `-peer` | — | server IPv4 (client only) |
| `-socks` | `127.0.0.1:1080` | local SOCKS5 listener |
| `-http` | — | optional local HTTP CONNECT proxy |
| `-L` | — | extra forwards `lport:host:rport,...` |
| `-linkid` | `0` | lane id 0–255; frames for another lane are dropped |
| `-payload` | `1300` | data bytes per frame (use 1350 for a 1450 MTU path) |
| `-fec` | `8` | one parity packet per K data packets; `0` disables |
| `-wnd` | `512` | receive window, in packets |
| `-pps` | `4000` | token bucket cap on outgoing ICMP per second |
| `-watchdog` | `15` | client: reset after N seconds stalled *with work pending* (`0` off) |
| `-suppress` | `true` | send an invalid ICMP checksum to suppress kernel echo |
| `-v` | `false` | verbose logging |

Exit code `3` from the watchdog is intentional: the process exits so that a supervisor
(systemd, launchd `KeepAlive`, a `while` loop on Windows) restarts it with a fresh session.
Do **not** try to keep the same process and swap sessions — a half-written frame from the old
session desynchronises the mux parser permanently.

## Troubleshooting

| Symptom | Most likely cause |
|---|---|
| client `rxfrm 0/s`, no echo requests in the server's tcpdump | path drops bad-checksum ICMP → `-suppress=false` |
| `rxpkt 0/s` + `rxfrm` looks healthy + `acked` frozen | session desync; wait for the watchdog, or restart both ends |
| `ooo` climbing into the hundreds | peer is waiting for a seq that will never come (same as above) |
| 0 B/s a few seconds to minutes *after* sustained speed | link throttling, not a protocol bug — check `ping -c 3 -s 1200 <server>`; see Limitations |
| first request after start hangs ~10 s | server still adopting the new session; warm up with one `curl` |
| walks at ~500 KB/s at 170 ms RTT | `-wnd`/SACK width too small for the path |

## Limitations

1. **The lane gets throttled.** After tens of MB at speed, large ICMP packets start dropping
   in bursts (`ping -s 1200` from 0% to 30%+ loss) and the tunnel reports 0 B/s for minutes.
   That is the path, not the code.
2. **`-suppress` is fatal on some paths** (see above). Diagnose with ping → server-side
   tcpdump → `rxfrm`, in that order.
3. **`-fec 2` and `-fec 4` do not work** — 0 B/s. Use `8` or `0`. Cause unknown; this is the
   biggest open question in the repo.
4. **`-dup` is useless**: the path dedupes by id/seq before the tunnel sees it.
5. **`-pps` is not the limiter** on the tested path: 4000 and 20000 behave identically.
6. **Cold start is slow** (~5–13 s for the first request): the server has to adopt the session.
7. **`kill -9` on the client can strand the server** on the old session; restart the server.
8. **Single lane only.** There is no automatic failover in the current tree; a dead path stays
   dead until it recovers or the watchdog restarts the client.

## Open questions

- Why does `-fec 2/4` produce 0 B/s while `8` and `0` work? The XOR path is shared.
- Why does throughput collapse after sustained load — pps shaping, queue occupancy, or the
  provider rate-limiting ICMP by payload size?
- Where is the ceiling on an RTT above 300 ms with the 256-bit bitmap?

## Comparison

| Project | Approach | Difference |
|---|---|---|
| [icmptunnel](https://github.com/DhavalKapil/icmptunnel) | tun/tap, IP packets inside ICMP (2016, C) | that one tunnels a whole IP interface; this one multiplexes TCP streams with FEC + SACK and ships a Windows raw-socket path |
| [ICMPTunnel](https://github.com/Qteam-official/ICMPTunnel) | reverse TCP forwarding | single forwarded port; this one is a general SOCKS5/HTTP exit |
| [udp2raw](https://github.com/wangyu-/udp2raw), [phantun](https://github.com/dndx/phantun) | camouflaging UDP/TCP as other protocols | same engineering problem family, different layer |

## Legal and ethics

This is a networking experiment and a protocol implementation. Tunnelling ICMP may violate
your ISP's terms of service, your hosting provider's acceptable-use policy, or local law.
Use it only on networks you own or are authorised to test, and check the rules that apply to
you before you do. The author takes no responsibility for misuse.

## License

Apache-2.0 — see [LICENSE](LICENSE). Copyright 2026 cyrainfall.

中文文档：[README.zh-CN.md](README.zh-CN.md)
