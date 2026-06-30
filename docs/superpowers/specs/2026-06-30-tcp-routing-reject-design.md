# TCP Routing Reject Design

## Goal

Add a main-routing `reject` action that actively refuses matching TCP
connections from LAN clients, without changing the existing `block` behavior.

The first version must:

- Treat `reject` as a built-in routing result alongside `direct`, proxy groups,
  and `block`.
- Preserve `block` as silent drop.
- Actively reject only LAN ingress TCP traffic.
- Return a TCP reset to the LAN client as early as possible in the eBPF/tc data
  path.
- Avoid sending rejected traffic to userspace or any proxy outbound.
- Keep UDP, DNS request routing, WAN egress, and gateway-local process behavior
  out of scope for active reject.
- Emit enough observability to distinguish `reject` from `block` during
  gateway diagnosis.

## Non-goals

The first version does not implement:

- UDP ICMP unreachable or any active UDP rejection.
- DNS request `reject`; that already exists and remains a separate DNS-routing
  feature.
- Any change to `block`, including changing it from drop to reject.
- Proxy-internal reject behavior.
- HTTP-layer error pages or TLS-level alerts.
- Active rejection for gateway-local process routing.
- Active rejection on WAN egress paths.
- A new user-facing policy engine or rule syntax beyond accepting `reject` as a
  top-level routing outbound.

## Terminology

### DNS request reject

`dns.routing.request` can already select `reject`. That rejects a DNS query
before a client opens a connection:

```dae
dns {
    routing {
        request {
            qname(geosite:category-ads-all) -> reject
        }
    }
}
```

This design does not change DNS request reject.

### Business routing reject

The top-level `routing` block decides what to do with TCP connections and UDP
flows that pass through the transparent gateway. This design adds `reject` as a
business-routing result:

```dae
routing {
    domain(suffix: bad.example) -> reject
    dip(203.0.113.0/24) -> reject
    fallback: proxy
}
```

For the first version, only TCP traffic from LAN clients receives an active TCP
reset. Non-TCP traffic that routes to `reject` keeps block-style drop behavior.

### Block versus reject

`block` and `reject` are separate built-in actions:

```text
block  = silent drop
reject = active TCP refusal for LAN ingress TCP
```

Rules that depend on stealth or leakage prevention should continue using
`block`. Rules that need fast client failure should use `reject`.

## User-Facing Semantics

The config parser and validator should accept `reject` anywhere the main
`routing` block currently accepts `block`.

Examples:

```dae
domain(suffix: example.org) -> reject
dport(25) && l4proto(tcp) -> reject
mac('00:11:22:33:44:55') && domain(suffix: blocked.test) -> reject
```

When a LAN client opens a TCP connection that matches a `reject` rule:

1. eBPF routes the packet and gets `OUTBOUND_REJECT`.
2. eBPF sends a TCP reset back toward the LAN client.
3. eBPF drops the original packet.
4. The packet is not redirected to the DAE control plane and no proxy dialer is
   selected.

Expected client behavior is a fast connection failure such as connection reset
or connection refused. The exact user-facing error text depends on the client
OS and application.

When non-TCP traffic matches `reject` in the first version, DAE drops it like
`block`. This avoids giving UDP a misleading active-reject guarantee before an
ICMP implementation is designed and tested.

## Architecture

Add one built-in outbound index:

```text
OUTBOUND_REJECT
```

The index is reserved like `OUTBOUND_BLOCK`, below the user-defined outbound
range. It is not a proxy group and does not perform dialing.

```text
LAN packet
  -> eBPF route()
  -> OUTBOUND_DIRECT  -> pass through
  -> user group       -> redirect to control plane when needed
  -> OUTBOUND_BLOCK   -> TC_ACT_SHOT
  -> OUTBOUND_REJECT  -> TCP RST for LAN ingress TCP, then TC_ACT_SHOT
```

The control plane still registers a built-in `reject` group so config
normalization, validation, routing metadata, and any userspace fallback path can
refer to the name. That group is not the primary enforcement point for LAN
transparent traffic.

## Configuration And Validation

The built-in main-routing outbounds become:

```text
direct
must_direct
block
reject
must_rules
```

Validation must:

- Accept `reject` in the top-level `routing` block.
- Continue accepting DNS request `reject` independently.
- Keep `reject` out of subscription and node selectors where a real outbound is
  required.
- Reject attempts to define a user group named `reject`, matching the existing
  reserved-name behavior for built-ins.

The docs must clarify that main-routing `reject` is TCP-only active reject in
the first version, while DNS request `reject` is a DNS-layer response.

## eBPF Data Path

### TCP reset helper

Add a helper in `control/kern/tproxy.c` that actively rejects a parsed LAN
ingress TCP packet.

The helper should:

- Only run for IPv4 or IPv6 TCP packets with a complete TCP header.
- Build a reset response by rewriting the current skb in place where possible.
- Swap Ethernet source and destination addresses when a link-layer header is
  present.
- Swap IP source and destination addresses.
- Swap TCP source and destination ports.
- Set TCP flags to RST and ACK when acknowledging a SYN or data packet.
- Compute the outgoing sequence and acknowledgement numbers according to TCP
  reset rules:
  - For incoming SYN without ACK, set `ack_seq = seq + 1`, set `seq = 0`, and
    set ACK.
  - For packets with ACK, use the incoming `ack_seq` as the outgoing `seq` and
    keep the reset valid for the peer.
  - Account for FIN and SYN consuming one sequence number.
  - Ignore payload trimming in the first implementation only if tests prove the
    reset is accepted; otherwise trim the skb to headers before sending.
- Recompute IP and TCP checksums.
- Redirect the rewritten packet back to the ingress interface toward the LAN
  client.
- Fall back to `TC_ACT_SHOT` if rewriting fails verifier checks or helper calls.

The implementation should prefer small, explicit helper functions over adding a
large inline block inside the routing decision. This keeps verifier failures and
review scope contained.

### Where to call reject

Every place that currently handles `OUTBOUND_BLOCK` on LAN ingress must consider
`OUTBOUND_REJECT`.

At minimum this includes:

- Existing TCP conn-state fast path.
- New TCP connection routing path after `route()`.
- Cached routing metadata path for established TCP packets.

The reject branch should run before outbound liveness checks and before control
plane redirection.

For non-LAN directions and non-TCP traffic, `OUTBOUND_REJECT` should fall back
to drop in the first version.

### Connection state

Rejected TCP connections should not create long-lived conn-state entries.

If conn state is created before route evaluation for a new TCP SYN, the reject
path should either:

- Remove the entry before returning, or
- Mark it closing and rely on existing cleanup only if tests prove it does not
  accumulate under repeated rejected SYNs.

The preferred behavior is to avoid persistent state for rejected SYN packets.

## Userspace Path

The control plane should register a built-in `reject` outbound group for
consistency, similar to `block`.

The userspace dialer for `reject` may immediately return `net.ErrClosed` or a
stable wrapped error. This is a fallback path and diagnostic aid, not the LAN
transparent enforcement mechanism.

Do not implement reject by routing to a proxy group and making the proxy fail.
That would blur policy semantics and make logs look like proxy failures instead
of routing decisions.

## Observability

Add a distinct event for active TCP reject:

```text
DAE_EVENT_REJECTED
```

The event should include the same fields already used by `DAE_EVENT_BLOCKED`:

```text
timestamp
type
pid
pname
outbound
l4proto
sip
dip
sport
dport
```

Userspace log rendering should make rejected traffic distinguishable from
blocked traffic. Operators should be able to answer:

- Which client was rejected?
- Which destination and port were rejected?
- Was the decision `block` or `reject`?
- Was the traffic TCP or a non-TCP fallback-to-drop case?

The first version does not require high-cardinality per-rule logging.

## Testing

### Parser and validation

Add tests that prove:

- Main routing accepts `-> reject`.
- `reject` is treated as a reserved built-in name.
- DNS request `reject` behavior remains independent.
- Existing `block` tests still pass unchanged.

### Constant synchronization

Add or update tests that prove Go and C eBPF constants agree:

- `OutboundReject`
- `OUTBOUND_REJECT`
- `OutboundUserDefinedMin`

The generator must be the source of truth for generated constants.

### eBPF behavior

Add focused eBPF tests for LAN ingress TCP:

- TCP SYN matching `reject` produces a reset packet toward the client.
- The original SYN is not forwarded to the control plane.
- TCP packets matching `block` are still silently dropped.
- A proxy-matching TCP SYN still follows the existing control-plane redirect
  path.
- Repeated rejected SYNs do not leak conn-state entries.

If full packet-output assertions are hard in the existing test harness, start
with a lower-level helper test for the RST rewrite plus an integration test that
observes fast client failure.

### Production acceptance

Before production deployment:

1. Build and test on the local Linux environment.
2. Validate production config with the candidate binary.
3. Add one temporary low-risk TCP-only reject rule for a test domain or test IP.
4. From a real LAN client, verify TCP connection failure is fast.
5. Verify DAE logs show a reject event.
6. Verify removing the test rule restores normal behavior.
7. Verify existing `block` rules still produce timeout-style drop behavior.

## Rollout

The first production rollout should not convert broad ad blocking rules to
`reject` automatically.

Recommended rollout sequence:

1. Deploy binary support for `reject` without adding broad production rules.
2. Validate a narrow temporary TCP rule from a LAN client.
3. Document the behavior in the operations runbook.
4. Only then consider replacing selected `block` rules with `reject`.

Broad ad filtering remains better suited to DNS request `reject` or a dedicated
DNS/ad-blocking layer when the operator wants DNS-level fast failure. Main
routing `reject` is the DNS-independent fallback for connection-level policy.

## Risks And Mitigations

### TCP reset correctness

Incorrect sequence or checksum handling can make clients ignore the reset and
fall back to timeout.

Mitigation: packet-level tests must inspect the returned RST and checksum. LAN
client acceptance must be part of rollout.

### Verifier complexity

Packet rewrite helpers can trigger eBPF verifier failures.

Mitigation: keep helpers small, avoid loops, bound all header accesses, and
fall back to drop when rewrite helpers fail.

### Semantic ambiguity

Users may assume `reject` covers UDP or DNS.

Mitigation: document first-version scope explicitly in config docs and operation
notes: main-routing `reject` actively rejects LAN ingress TCP only.

### Existing block expectations

Some users may rely on `block` being silent.

Mitigation: do not change `block`; introduce `reject` as a separate action.

## Open Decisions

All first-version decisions are fixed by this design:

- `block` remains silent drop.
- `reject` is a main-routing built-in action.
- Active rejection is limited to LAN ingress TCP.
- UDP `reject` falls back to drop in the first version.
- DNS request `reject` remains separate and unchanged.
