# Native IPv4 FakeIP Design

## Goal

Add a native IPv4 FakeIP mode to dae's existing DNS server and transparent
gateway so proxied domains no longer require a locally reachable foreign DNS
upstream before traffic can be routed.

The first version must:

- Keep dae as the LAN-facing DNS server.
- Let `dns.routing.request` explicitly choose between real DNS upstreams,
  `fakeip`, `asis`, and `reject`.
- Return stable synthetic IPv4 addresses for queries routed to `fakeip`.
- Preserve dae's existing business `routing` rules and eBPF routing model.
- Preserve dae's kernel fast path for ordinary direct traffic. Adding FakeIP
  must not turn real-IP direct traffic into a userspace-forwarded path.
- Route each connection exactly once using the existing business routing
  pipeline and the connection's complete metadata.
- Send the original domain name to proxy outbounds without resolving that
  domain locally.
- Resolve a FakeIP domain locally only when the final business outbound is
  `direct` or `must_direct`.
- Persist mappings across reloads and process restarts.
- Prevent FakeIP addresses from ever being forwarded directly to the public
  network.

The production target configuration uses FakeIP by default for public domains
while continuing to use real DNS for China domains, local zones, proxy-node
hostnames, and explicitly excluded domains.

## Terminology

This design distinguishes two independent routing stages:

### DNS request routing

`dns.routing.request` runs when dae receives a DNS query. It decides how dae
answers that query:

- Query a named real DNS upstream.
- Return a FakeIP.
- Forward the query as-is.
- Reject the query.

DNS request routing does not select the traffic outbound.

### Business traffic routing

The top-level `routing` block runs for a TCP connection or UDP flow. It decides
the final outbound using all available metadata, including:

- Domain-rule bitmap.
- Source and destination addresses.
- Source and destination ports.
- TCP or UDP protocol.
- Client MAC address.
- Process name where available.
- Marks and other existing routing metadata.

Business routing runs once per new connection or UDP flow. FakeIP does not add
a second business-routing pass.

## Existing Real-IP Flow

The existing domain-routing flow is:

```text
client queries domain
  -> dns.routing.request selects a real DNS upstream
  -> upstream returns real IP
  -> dae computes the domain-rule bitmap for the domain
  -> dae associates the real IP with that bitmap
  -> client connects to the real IP
  -> eBPF combines the bitmap with connection metadata
  -> business routing selects the outbound
```

The foreign DNS lookup currently has two responsibilities:

1. Return an address usable by the client.
2. Give dae an address on which to attach the domain-rule bitmap.

If the foreign DNS upstream itself uses a failed proxy node, new domains do not
receive addresses and the traffic path cannot begin.

## FakeIP Flow

For `www.google.com`, the new flow is:

```text
client queries A www.google.com
  -> dns.routing.request selects fakeip
  -> dae allocates 198.18.0.10
  -> dae stores www.google.com <-> 198.18.0.10
  -> dae computes and stores the domain-rule bitmap for 198.18.0.10
  -> dae returns 198.18.0.10 without querying foreign DNS

client connects to 198.18.0.10:443
  -> eBPF recognizes the FakeIP range and forces control-plane handling
  -> existing business routing uses the stored bitmap and connection metadata
  -> selected outbound is proxy_failover
  -> userspace looks up 198.18.0.10 -> www.google.com
  -> dae sends www.google.com:443 to the selected proxy dialer
  -> the proxy side resolves and connects to the real destination
```

The reverse FakeIP lookup restores the proxy dial target. It does not trigger a
second business-routing pass.

## Configuration

Example:

```dae
dns {
    ipversion_prefer: 4

    fakeip {
        enabled: true
        inet4_range: 198.18.0.0/15
        ttl: 60
        store: /var/lib/dae/fakeip.db
        direct_upstream: cn
    }

    upstream {
        localdns: 'tcp+udp://127.0.0.1:5354'
        cn: 'https://223.5.5.5:443/dns-query'
    }

    routing {
        request {
            subnode() -> cn
            qname(suffix: home.arpa, suffix: local) -> localdns
            qname(suffix: cn) -> cn
            qname(geosite:cn) -> cn
            fallback: fakeip
        }
    }
}
```

### `fakeip` request outbound

`fakeip` is a built-in DNS request outbound. It is available only when
`dns.fakeip.enabled` is true and is used alongside existing named upstreams
and the built-in `asis` and `reject` outbounds.

It may appear on the right-hand side of ordinary `qname` or `qtype` request
rules and as the ordinary DNS request fallback:

```dae
qname(suffix: example.com) -> fakeip
fallback: fakeip
```

Internal selectors such as `sub()`, `node()`, and `subnode()` must not select
`fakeip`. Subscription and proxy-node bootstrap resolution requires real IP
addresses.

### Configuration fields

`enabled`

- Boolean.
- Default: `false`.

`inet4_range`

- IPv4 prefix used for synthetic addresses.
- Default: `198.18.0.0/15`.
- Must not overlap configured LAN addresses, node addresses, DNS listener
  addresses, or another dae-managed address range.
- Startup activation must also reject overlap with the host's effective local
  routes.
- Network and broadcast-style boundary addresses are reserved and never
  allocated.

`ttl`

- TTL returned in synthetic A records.
- Default: `60` seconds.
- TTL expiration does not release a mapping.

`store`

- Persistent database path.
- Default: `/var/lib/dae/fakeip.db`.

`direct_upstream`

- Name of a real upstream declared in `dns.upstream`.
- Required when FakeIP is enabled.
- Used only when business routing selects `direct` or `must_direct` for a
  FakeIP destination.
- Must not refer to `fakeip`, `asis`, or `reject`.
- Direct resolution bypasses `dns.routing.request` to avoid recursively
  selecting `fakeip`.

### Validation

Configuration validation must fail when:

- `fakeip` is referenced while `dns.fakeip.enabled` is false.
- `inet4_range` is invalid, is not IPv4, has insufficient usable addresses, or
  overlaps a statically known local or dae-managed range.
- `ttl` is zero or negative.
- `store` is empty.
- `direct_upstream` is absent, undefined, or not a real DNS upstream.
- An internal DNS selector routes to `fakeip`.
- IPv6 FakeIP fields are supplied in the first-version configuration.

## DNS Behavior

### A queries

For a query routed to `fakeip`:

1. Canonicalize the queried name using DNS case-insensitive semantics and a
   consistent trailing-dot representation.
2. Return the existing mapping if one exists.
3. Otherwise allocate the next never-before-used address.
4. Persist both directions and allocator state transactionally.
5. Compute the current business domain-rule bitmap for the domain.
6. Publish the FakeIP-to-bitmap association.
7. Return one synthetic A record with the configured TTL.

The DNS response must not be sent until persistence succeeds.

### AAAA queries

The first version has no IPv6 FakeIP support. An AAAA query routed to `fakeip`
returns a successful empty answer.

### Other query types

The first version synthesizes only A responses. Every non-A query routed to
`fakeip`, including TXT, MX, SRV, HTTPS, and CNAME, returns a successful empty
answer. The implementation must not contact an upstream as a hidden fallback.
Domains that require real non-address records must be routed explicitly to a
real DNS upstream.

### Real DNS exclusions

The production policy uses explicit DNS request rules for domains that require
real answers:

- Subscription and proxy-node hostnames.
- `home.arpa`, `.local`, and other local zones.
- `.cn` and `geosite:cn`.
- Any user-specified compatibility exclusions.

DNS request routing remains independent from business routing. Operators may
explicitly select any configured real upstream for any domain without changing
the traffic-routing rules.

## Mapping and Address Allocation

The FakeIP store maintains:

```text
canonical domain -> FakeIP
FakeIP -> canonical domain
allocator cursor
database format version
configured IPv4 prefix identity
```

First-version allocation semantics:

- A domain keeps the same FakeIP permanently.
- An allocated FakeIP is never automatically returned to the pool.
- An address is never assigned to a different domain.
- Reload and restart preserve all mappings.
- TTL expiration does not remove mappings.
- DNS-cache LRU eviction does not remove FakeIP mappings.
- Pool exhaustion returns `SERVFAIL` for newly unmapped domains.
- Pool exhaustion never steals or overwrites an existing mapping.

`198.18.0.0/15` provides approximately 131,000 usable addresses, which is
expected to be sufficient for the initial home-gateway deployment. Status
output must expose capacity and allocation count so growth is observable.

## Persistence

The store is long-lived state shared across control-plane generations.

Requirements:

- Use a transactional embedded key-value database.
- Prefer an existing suitable project dependency; otherwise use bbolt.
- Use one serialized writer while allowing concurrent lookups.
- Commit the domain mapping, reverse mapping, and allocator cursor in one
  transaction.
- Open and validate the store before accepting DNS queries.
- Close the database cleanly during process shutdown.
- Never delete mappings during normal shutdown.

Startup validation checks:

- Database format version.
- Stored prefix identity matches configured `inet4_range`.
- Every address is inside the usable pool.
- Domain-to-IP and IP-to-domain mappings are bijective.
- No duplicate domain or address ownership exists.

If validation or database opening reports corruption:

1. Close the database if possible.
2. Rename it to `fakeip.db.corrupt-<timestamp>`.
3. Create a new empty database.
4. Log a high-priority error containing the isolated path.
5. Continue dae startup.

After rebuilding an empty database, connections to unknown FakeIP addresses
are rejected. They must never be forwarded directly.

If a normal write transaction fails, the DNS query returns `SERVFAIL`; dae
must not return an address backed only by volatile memory.

## Reload Semantics

Normal reload:

- Reuses the same open FakeIP store.
- Preserves all mappings and the allocator cursor.
- Recomputes the domain-rule bitmap for every allocated domain.
- Republishes FakeIP-to-bitmap associations before the replacement generation
  becomes active.
- Allows changes to `ttl` and `direct_upstream`.

Changing `inet4_range` or `store` is not hot-reloadable:

- `dae reload` rejects the change.
- The running generation remains active.
- The error instructs the operator to perform a full restart.

The DNS configuration fingerprint and reload-reuse contract must include all
FakeIP fields.

## eBPF and Routing Behavior

### FakeIP safety boundary

All TCP and UDP traffic whose destination is inside the configured FakeIP
prefix must enter dae userspace.

The eBPF data plane must:

- Recognize the configured FakeIP prefix.
- Never apply kernel direct-forwarding to a FakeIP destination.
- Redirect FakeIP traffic to the existing transparent proxy control-plane
  path.
- Preserve the domain-rule bitmap and all existing connection metadata for
  business routing.

This forced userspace path applies even if a rule would otherwise select
`direct` or `must_direct`.

### Known performance issue: FakeIP selected as direct

This forced path creates an important first-version performance exception:

- Proxy traffic already requires dae userspace to implement the proxy
  protocol, so FakeIP reverse lookup adds little structural overhead.
- Ordinary real-IP traffic selected as `direct` remains in the existing eBPF
  kernel fast path and must not be redirected to userspace.
- A FakeIP destination whose business result is `direct` or `must_direct`
  cannot use that kernel fast path. Userspace must currently recover the
  domain, resolve a real address, and relay the connection.

The third case loses one of dae's primary architectural advantages. It is a
correctness and safety fallback, not an acceptable steady-state design for a
large portion of direct traffic.

The first version therefore has this operational invariant:

```text
domain expected to be direct
  -> dns.routing.request selects a real DNS upstream
  -> client connects to a real IP
  -> eBPF selects direct
  -> kernel forwards without dae userspace relay

domain selected for FakeIP
  -> business routing should normally select a proxy or block
```

Operators must keep known direct domains outside FakeIP through explicit DNS
request rules. A `FakeIP + direct` result indicates incomplete agreement
between DNS request routing and business routing, or a business decision that
depends on metadata unavailable at DNS-query time.

This limitation must remain visible in implementation logs, tests, and
documentation. Future work must reduce or eliminate the `FakeIP + direct`
intersection rather than accepting broad userspace forwarding as the final
architecture.

### Business routing

When a FakeIP is allocated, dae computes the same domain-rule bitmap that it
currently computes for real DNS answers and associates that bitmap with the
FakeIP.

When the client connects:

1. Existing business routing evaluates the bitmap together with the complete
   connection metadata.
2. It selects the final outbound exactly once.
3. Userspace reverse-looks up the FakeIP to recover the original domain for
   dialing.
4. Userspace preserves the already selected outbound and does not rerun the
   complete business routing program.

### Unknown FakeIP

If a destination is inside the FakeIP prefix but has no reverse mapping:

- Reject the connection or UDP flow.
- Emit a rate-limited warning.
- Do not attempt SNI-based recovery as an implicit fallback.
- Do not send the synthetic destination to any direct or proxy dialer.

## Outbound Behavior

### Proxy outbounds

For `proxy`, `proxy_failover`, and other proxy-backed groups:

- Dial `canonical-domain:original-port`.
- Do not resolve the business domain locally.
- Let the selected proxy protocol and remote side resolve the domain.
- Preserve existing group selection, failover, retry, and health behavior.

The proxy node's own hostname continues to use real bootstrap DNS selected by
`subnode()` or `node()` rules.

### Direct outbounds

For `direct` and `must_direct`:

1. Resolve the recovered domain through `dns.fakeip.direct_upstream`.
2. Do not pass this lookup through `dns.routing.request`.
3. Select a real address using existing IP-version and connection-family
   policy, constrained to IPv4 in the first version.
4. Connect directly to that real address and the original destination port.

Resolution failure rejects the connection. It does not fall back to a proxy,
another DNS upstream, or the FakeIP address.

This is primarily a safety path for DNS and business-rule disagreement or
business rules whose final result depends on port, protocol, source, MAC, or
process metadata. Operators should normally exclude known direct domains from
FakeIP through explicit DNS request rules.

This safety path is intentionally not considered performance-equivalent to
dae's normal direct path. It must not be used to justify routing all public
domains to FakeIP without measuring how many flows later resolve to `direct`.

### Blocked traffic

Existing block and reject business-routing behavior remains authoritative.
Blocked FakeIP traffic is rejected without any real DNS lookup.

## TCP and UDP

### TCP

- Reverse lookup occurs before proxy dial-target construction.
- TLS or HTTP sniffing is not required to recover the domain.
- Existing sniffing may continue for diagnostics or policy compatibility, but
  it must not override a valid FakeIP mapping.
- Connection logs include both the synthetic destination and recovered domain
  at debug level without changing normal info-log cardinality unnecessarily.

### UDP

- FakeIP UDP flows use the same reverse lookup and selected business outbound.
- Proxy UDP sends a domain target when the selected protocol supports domain
  destinations.
- Direct UDP resolves through `direct_upstream` before creating the endpoint.
- UDP endpoint identity must retain enough FakeIP mapping identity to prevent
  an endpoint from being reused for a different domain.
- Existing UDP-session and failover lifetime behavior remains unchanged.
- Tests cover ordinary UDP and QUIC destinations.

DNS traffic to dae's listener remains handled by the DNS fast path and must not
be confused with ordinary UDP traffic whose destination happens to be a
FakeIP.

## Controller Ownership

The FakeIP store lifetime follows the existing shared DNS-controller store
model:

- One long-lived store survives ordinary configuration reloads.
- Generation-local controller facades reference the shared store.
- Shutdown closes the store only after DNS listeners and active generation
  users stop.
- A replacement generation cannot observe a nil or partially initialized
  FakeIP store during handoff.

The implementation should integrate with the current DNS-controller reuse
mechanism rather than introduce an unrelated global singleton.

## Observability

Status output must include:

```text
fakeip_enabled: true
fakeip_cidr: 198.18.0.0/15
fakeip_allocated: 1234
fakeip_capacity: 131070
fakeip_database: /var/lib/dae/fakeip.db
fakeip_database_status: healthy
```

Logs must cover:

- Store opened and restored mapping count.
- New allocation at debug level.
- Pool utilization milestones at info or warning level.
- Pool exhaustion.
- Unknown FakeIP traffic.
- Database transaction failure.
- Database corruption and isolation path.
- Direct-upstream resolution failure.
- Reload rejection caused by CIDR or store-path changes.

Logs must not expose unrelated DNS payloads, subscription URLs, proxy
credentials, or other secrets.

## Failure Behavior

- FakeIP pool exhausted: return DNS `SERVFAIL` for new domains.
- Persistent write failed: return DNS `SERVFAIL`.
- Reverse mapping absent: reject traffic.
- `direct_upstream` lookup failed: reject traffic.
- Proxy group unavailable: preserve existing outbound error and failover
  semantics.
- Database corrupt at startup: isolate, rebuild, continue startup, and reject
  unknown old FakeIPs.
- FakeIP configuration invalid: fail validation before reload or startup.
- CIDR or database path changed during reload: reject reload and keep the old
  generation active.

No failure path may forward a FakeIP address to the public network.

## Testing

### Configuration tests

- Parse, marshal, and outline all FakeIP fields.
- Reject invalid IPv4 ranges and overlaps.
- Reject missing or invalid `direct_upstream`.
- Reject `fakeip` when disabled.
- Reject internal selectors targeting `fakeip`.
- Verify the DNS reload fingerprint covers every FakeIP field.

### Allocator and persistence tests

- Stable domain-to-IP allocation.
- Case-insensitive domain canonicalization.
- Unique address ownership.
- Transactional forward/reverse mapping.
- Restart restoration.
- Pool exhaustion without reuse.
- Write-failure behavior.
- Corruption isolation and empty-store recovery.
- Prefix mismatch detection.

### DNS tests

- Request routing to `fakeip`, a named real upstream, `asis`, and `reject`.
- A query returns the stable synthetic address and configured TTL.
- AAAA query returns a successful empty answer.
- FakeIP response does not call a foreign DNS forwarder.
- Real-DNS exclusion rules retain current behavior.
- Concurrent queries for the same domain allocate one address.

### Routing and eBPF tests

- FakeIP destinations are always redirected to userspace.
- FakeIP bitmap participates in existing domain routing.
- Port, protocol, source, MAC, and process predicates still affect final
  routing.
- Business routing runs once for a new FakeIP flow.
- Unknown FakeIP traffic is rejected.
- A FakeIP destination can never use kernel direct forwarding.

### Outbound tests

- Proxy TCP dials the recovered domain without local business-domain DNS.
- Proxy UDP uses the recovered domain.
- Failover selects the fallback while preserving the domain target.
- Direct TCP and UDP resolve only through `direct_upstream`.
- Direct-resolution failure does not fall back.
- Block rules do not trigger direct DNS resolution.

### Reload tests

- Mapping and allocator state survive reload.
- Existing mappings receive recomputed domain bitmaps.
- TTL and direct-upstream changes reload successfully.
- CIDR and store-path changes reject reload.
- DNS listener handoff never observes an unavailable FakeIP store.

### Production acceptance

On a real LAN client:

1. Query a China domain and confirm a real address from `cn`.
2. Query a local domain and confirm the `localdns` result.
3. Query a new proxy domain and confirm a FakeIP response without an external
   business-domain DNS lookup.
4. Connect to that domain and confirm logs show the configured proxy outbound
   and a domain dial target.
5. Make the primary proxy node unavailable.
6. Query another previously unseen proxy domain.
7. Confirm DNS still returns a FakeIP.
8. Confirm the new connection uses the failover proxy and succeeds without a
   foreign DNS upstream.
9. Exercise a FakeIP flow whose complete business rule selects `direct` and
   confirm only `direct_upstream` resolves it.
10. Restart dae and confirm a previously allocated domain retains its FakeIP.

## First-Version Non-goals

The first version does not implement:

- IPv6 FakeIP.
- Automatic address reclamation.
- Reuse of previously allocated addresses.
- Multiple FakeIP pools.
- An external FakeIP DNS daemon.
- Online database migration or partial corruption repair.
- A management API.
- Automatic selection of FakeIP versus real DNS from business routing.
- Client connection migration.

## Future Directions

### DNS `auto` mode

A future DNS request outbound may infer FakeIP versus real DNS from business
domain rules:

```dae
fallback: auto
```

This is intentionally excluded from the first version. A correct design must
define deterministic behavior for:

- Rules combining domain with port or protocol.
- Source, MAC, process, and mark predicates unavailable at DNS-query time.
- Rule order and fallback interaction.
- Domains that can select different outbounds for different connections.

Until those semantics are defined, explicit `dns.routing.request` rules remain
the authoritative DNS-answer policy.

### Preserve the eBPF direct fast path

The preferred optimization direction is to prevent domains that will use
`direct` from receiving FakeIP answers, so their traffic continues to use
dae's real-IP eBPF fast path.

The future design must:

- Reuse business domain rules where their result is deterministic at DNS-query
  time.
- Keep explicit DNS routing available for rules whose result depends on port,
  protocol, source address, MAC address, process name, mark, or other
  connection-only metadata.
- Expose the number of `FakeIP + direct` flows so configuration disagreement is
  measurable.
- Avoid silently converting ordinary direct traffic into permanent userspace
  relaying.
- Preserve the rule that proxy domains are sent to the proxy as domains and do
  not require local foreign DNS resolution.

`fallback: auto` is one possible mechanism, but it is not sufficient by itself
for metadata-dependent business rules. The optimization requires an explicit
policy for ambiguous domains, such as requiring a DNS override or choosing a
conservative real-DNS answer.

### Safe reclamation

A future allocator may reclaim addresses using:

- Last DNS query time.
- Last connection time.
- Active TCP and UDP session references.
- A configurable retention period.
- A quarantine period before address reuse.

Reclamation must never allow a client holding a stale DNS answer to reach a
different domain.

### Additional operations

Future versions may add:

- IPv6 FakeIP.
- Database inspection, export, verification, migration, and explicit reset
  commands.
- Runtime metrics and management APIs.
- Multiple pools selected by DNS rules.
- More detailed allocation and traffic counters.

## Documentation Updates After Implementation

When the feature is implemented, update:

- `example.dae`
- `docs/en/configuration/dns.md`
- `docs/zh/configuration/dns.md`
- `docs/zh/how-it-works.md`
- `docs/zh/architecture.md`

Current user documentation must describe only implemented behavior. Future
directions remain in this design document until they are delivered.
