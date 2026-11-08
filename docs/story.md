# From 0 to 1.3 MB/s: building a tunnel on the one protocol that still worked

This is the long version of the [README](../README.md): why this program exists, what
failed before it, and the five fixes that took it from nothing to 1.3 MB/s on a 170 ms
path. Every number here was measured on a real link, not estimated.

If you only read one paragraph: on some paths TCP and UDP are interfered with
selectively while ICMP echo still passes end to end. `icmptun` carries a TCP proxy inside
ICMP, and the thing that decides its throughput is not the payload size, the window or the
packet rate — it is **the width of the SACK bitmap in the frame header**. Going from 64 bits
to 256 bits multiplied throughput by 2.7.

---

## 1. The trail of rejections

The starting point was ordinary: a laptop that needed a working exit. Seven options were
tried and rejected before this program existed. The order matters, because each rejection
taught something the next attempt needed.

| Attempt | Verdict | Why it failed |
|---|---|---|
| Borrow a colleague's machine as the exit | Withdrawn | Not ours to keep; retired deliberately for safety |
| A cloud host that could reach the internet directly | ✗ | Its whole TCP path to the destination was unreachable; DNS was poisoned |
| A cloud vendor's "global accelerator" | ✗ | Needed a filing/registration and a cross-border leased line, ~$60/month |
| Modern proxy protocols (TLS-wrapped, REALITY, Trojan) | ✗ | The TCP handshake itself never completed |
| QUIC-based transport (Hysteria2) | ✗ | UDP delivery was 6–26% — unusable |
| An older UDP KCP tunnel | ✗ | Project gone from GitHub, packaging archived |
| A well-known ICMP tunnel (`ptunnel-ng`) | ⚠️, then ✗ | It worked — at 40–175 KB/s. A 64-packet go-back-N window at 170 ms RTT is a hard ceiling of about 500 KB/s, and it was nowhere near that |
| **Write our own ICMP + FEC tunnel** | ✓ | **1.29–1.39 MB/s**, 3.8× the relay that had been used until then |

The lesson that unlocked everything: **"TCP handshakes but data stalls" is not a
configuration problem.** Ping worked, banners arrived, third-party reachability checkers
were all green — and the first real transfer died. That signature is selective
interference at the payload level, and no amount of proxy configuration fixes it.

Diagnosis, in order, once that was suspected:

1. `ping` and `traceroute` — full path, 0% loss.
2. Plain TCP connect to a service on the far host — handshake completes.
3. Send actual bytes over that connection — stalls after the first response.
4. Same test from a *different* network — works perfectly.
5. Raw ICMP throughput test — 2.2–3.7 MB/s, clean.

That is the entire premise of this project: **TCP and UDP are eaten, ICMP is not.**

## 2. The design

One frame per ICMP echo packet, 60 bytes of header plus payload:

```
 0     4   5   6        10       14       18    20 ┌──────────── 52 ┐ 52   56  57   59
[magic][ver][flags][session][seq][cum-ack][wnd] [ 256-bit SACK  ] [blk][pi][len][link]
```

Frames are multiplexed into streams with a 9-byte sub-header
(`[type][stream:4][len:4][data]`, `type` = open/data/close/credit), so one tunnel carries
many TCP flows.

Default payload is 1350 B, giving `60 + 1350 + 8 + 20 = 1438 B` on the wire — under the
common 1450 B MTU. Going over that ceiling does not error, it just silently drops to
0 B/s, which costs an hour the first time you discover it.

## 3. Five fixes, from zero to 1.3 MB/s

Each one below was a real symptom that looked like a different bug.

### ① No packets came back at all — mirror the peer's ICMP id/seq

The server was answering with its own random ICMP identifier and sequence number. On a
CGNAT'd path, replies are only forwarded if they match an outstanding outbound request, so
every single reply was dropped in the NAT. The server now copies the most recent id/seq it
observed from the client into its own replies. Connectivity returned immediately.

### ② Restarting the client wedged the tunnel — advertise and accept a new session

A restarted client begins at sequence 0. The server was still tracking the previous
session and treated every new frame as a duplicate of something long since received, so
both ends sat waiting for the other. The fix is a session field that the server compares
against its own: a mismatch resets all window state instead of discarding the frame.

### ③ Retransmission storm — the bitmap polarity and its base

Symptom: 89,000 retransmitted packets in five seconds, effective throughput 140 KB/s.

Two bugs compounded:

- the hole bitmap was computed from `rcvNext+1` instead of `rcvNext`, which hides a gap
  exactly at the front of the window — the single most important one to report;
- every ACK re-reported the same hole, so the sender retransmitted the same packet dozens
  of times per round trip.

Fix: bit 0 of the bitmap is `rcvNext`; retransmissions are de-duplicated; at most four
packets are added per RTO; and a minimum gap between retransmissions of the same sequence
number.

### ④ Asymmetric throughput — the receive buffer was the bottleneck

Upload reached 604 KB/s while download stalled at 130 KB/s. Upload working and download
not points at the *receive* path, not the send path. macOS raw sockets default to a
receive buffer far too small for a 4096-packet window; raising `SO_RCVBUF` to 8 MB (and
`SO_SNDBUF` to 4 MB) took download to 318 KB/s instantly.

### ⑤ The real ceiling — 64 bits of SACK were not enough (decisive)

With cumulative ack stuck behind one lost packet, the only information the sender has
about everything after the gap is the SACK bitmap. Its width therefore *is* the bandwidth
at high RTT:

```
64 bits × 1350 B / 0.17 s ≈ 500 KB/s      ← exactly the ceiling we kept hitting
```

Widening the bitmap to 256 bits (headers grew from 36 to 60 bytes) moved the same
configuration to **1.31–1.39 MB/s**.

This is the most counter-intuitive result in the project: **widening a bitmap by 28 bytes
did more than every window, packet-rate and FEC adjustment combined.**

## 4. Three silent killers

These never produce an error. They produce healthy-looking logs and zero throughput.

**Credit leak.** Flow control used a 256 KB credit window, debited per 8 KB read and
credited per received frame — but dropped packets never returned credit. The window only
ever shrank, so after a few MB the server blocked forever. Symptom: server tx = 0,
client rx = 0, keepalive still flowing. Fix: effectively remove the credit scheme and
push backpressure into the write queue instead.

**Deliberately corrupt checksums.** To stop the remote kernel from auto-replying to every
frame (which doubles the packet count and fights the NAT mapping), the client writes an
invalid ICMP checksum. It works — until a path drops bad-checksum ICMP outright. Then
nothing is sent at all: `rxfrm 0/s` on the client and not a single echo request in the
server's packet capture. `-suppress=false` fixed it on exactly one of the networks we use.

**Liveness measured the wrong thing.** A watchdog that asks "did a frame arrive?" cannot
see a dead session: after the peer resets its state, its frames still flow, `rxfrm` looks
great, the in-flight count never clears, and `acked` never advances. The watchdog now
measures *progress* — did `sndUna` or `rcvNext` move — and exits with code 3 so a
supervisor restarts it with a fresh session. Restarting the process is the only safe
recovery: reusing a half-parsed session permanently desynchronises the mux parser.

## 5. Porting to Windows

Four things, in order of pain:

- `syscall.Sendto` / `Recvfrom` are `return EWINDOWS` stubs on Windows. The platform file
  uses `net.ListenIP("ip4:icmp")` and `ReadFromIP`/`WriteToIP` instead.
- Raw sockets need an elevated process.
- If a full-tunnel VPN is also running, its `0.0.0.0/1` + `128.0.0.0/1` half-routes
  outrank the default route, so the tunnel's own ICMP gets swallowed and the tunnel wraps
  around itself. It needs an explicit host route to the peer via the physical gateway.
- Testing failover by killing the process is meaningless when a supervisor restarts it in
  three seconds. Block ICMP with a firewall rule instead.

## 6. What it actually measures

Same configuration (`-wnd 4096 -fec 8 -payload 1350 -pps 20000`) on a ~170 ms path:

| Path | Direction | Result |
|---|---|---|
| macOS client | download | 1.24–1.39 MB/s |
| macOS client | upload | 2.09 MB/s |
| Windows client | download | 0.63–1.11 MB/s |
| Windows client | upload | 3.25 MB/s |
| TLS relay on the same pair, for comparison | download | 360 KB/s |
| Bare ICMP, no tunnel | — | 2.2–3.7 MB/s |

The tunnel reaches about 60% of the weaker end of the raw ICMP range; the rest is FEC
overhead and loss.

## 7. What is still unknown

- `icmptun -fec 2` and `-fec 4` produce **0 B/s** while `8` and `0` work. They share the
  same XOR path, so this is not obviously a coding bug. Unexplained.
- Throughput collapses for minutes after sustained load, and the collapse is independent
  of packet rate (4000 and 20000 pps behave identically). Path-level shaping, most
  likely, but not proven.
- No measurement above 300 ms RTT yet, so the 256-bit ceiling there is extrapolated.

## 8. The one-line versions

- **Diagnosis:** ping fine, banner fine, data stalls → selective interference, not config.
- **Workaround:** TCP dead, UDP crippled, ICMP alive → put TCP inside ICMP.
- **Speed:** widen the SACK bitmap; it beats every other knob.
- **Robustness:** assume the session will wedge; supervise the process instead of hoping
  the protocol heals itself.
