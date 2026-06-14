# Native IPv4 FakeIP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add native IPv4 FakeIP DNS answers and transparent TCP/UDP forwarding while preserving the existing independent DNS-routing and business-routing pipelines.

**Architecture:** `dns.routing.request` gains a built-in `fakeip` outbound backed by a persistent, append-only IPv4 mapping store. DNS answers publish the domain bitmap against the synthetic address, eBPF redirects FakeIP destinations to userspace while preserving its first and only business-routing result, and userspace reverses the synthetic address only to construct the dial target. Proxy outbounds receive the original domain. Ordinary real-IP direct traffic must retain dae's eBPF kernel fast path; the first-version `direct_upstream` handling is only a safety fallback for the undesirable `FakeIP + direct` intersection and is explicitly tracked as future optimization debt.

**Tech Stack:** Go 1.26, miekg/dns, cilium/ebpf, bbolt, Linux TC eBPF, existing dae DNS controller and TCP/UDP endpoint pool, Go tests, BPF C tests.

---

## File Map

- Modify `config/config.go`
  - Add the `dns.fakeip` configuration model.
- Modify `config/patch.go`
  - Apply FakeIP defaults.
- Modify `config/desc.go`
  - Document FakeIP fields in generated descriptions.
- Modify `config/outline.go`
  - Emit the FakeIP section in config outlines.
- Modify `config/*_test.go`
  - Cover decoding, defaults, marshaling, and validation.
- Modify `cmd/reload_manager.go`
  - Fingerprint FakeIP configuration and reject incompatible live reloads.
- Modify `cmd/run_shutdown_test.go`
  - Keep reflection-based DNS fingerprint coverage passing.
- Modify `common/consts/dns.go`
  - Add the reserved DNS request outbound `fakeip`.
- Modify `component/dns/request_routing.go`
  - Parse `fakeip` in request rules and fallback.
- Modify `component/dns/dns.go`
  - Return `fakeip` as a built-in selection and expose named upstream lookup.
- Modify `component/dns/*test.go`
  - Cover FakeIP request routing and named upstream lookup.
- Add `control/fakeip_store.go`
  - Own persistent domain/IP mappings, allocation, reverse lookup, iteration, and statistics.
- Add `control/fakeip_store_test.go`
  - Cover persistence, atomicity, exhaustion, corruption recovery, and concurrency.
- Modify `control/dns_control.go`
  - Synthesize FakeIP DNS responses and resolve direct FakeIP traffic through a selected named upstream.
- Add `control/dns_fakeip_test.go`
  - Cover A, AAAA, non-A, cache behavior, failures, and direct resolution.
- Modify `control/control_plane.go`
  - Initialize/reuse the store, publish mapping bitmaps, and expose reverse lookup.
- Modify `control/control_plane_reload_test.go`
  - Cover mapping replay and store ownership through reload.
- Modify `control/kern/tproxy.c`
  - Detect the configured IPv4 FakeIP prefix and force userspace interception without changing the routing result.
- Modify `control/bpf_utils.go`
  - Rewrite FakeIP constants into BPF `.rodata`.
- Regenerate `control/bpf_bpfel.go`, `control/bpf_bpfeb.go`, and BPF objects with `make ebpf`.
- Modify `control/bpf_stub.go`
  - Keep stub ABI aligned with `struct dae_param`.
- Modify `control/kern/tests/bpf_test.c`
  - Cover direct, proxy, block, TCP, UDP, and non-FakeIP interception.
- Modify `control/dial.go`
  - Support an authoritative domain target that never triggers business rerouting.
- Modify `control/tcp.go`
  - Reverse FakeIP before sniffing and handle proxy/direct targets.
- Add `control/tcp_fakeip_test.go`
  - Cover proxy, direct, unknown FakeIP, and routing-result preservation.
- Modify `control/udp.go`
  - Reverse FakeIP before QUIC sniffing, preserve destination affinity, and dial the domain or direct real IP.
- Modify `control/udp_flow.go`
  - Force destination-affine endpoint keys for FakeIP flows.
- Add `control/udp_fakeip_test.go`
  - Cover UDP proxy/direct behavior, QUIC, endpoint isolation, and unknown mappings.
- Modify `docs/configuration/dns.md`
  - Document `dns.fakeip`, routing semantics, direct resolution, and reload constraints.
- Modify `example.dae`
  - Add a commented FakeIP example.

### Task 1: Define and validate the FakeIP configuration contract

**Files:**
- Modify: `config/config.go`
- Modify: `config/patch.go`
- Modify: `config/desc.go`
- Modify: `config/outline.go`
- Test: `config/decode_test.go`
- Test: `config/marshal_test.go`
- Test: `cmd/run_shutdown_test.go`

- [ ] **Step 1: Write failing decode and default tests**

Add a complete configuration fixture:

```go
func TestDecodeDNSFakeIP(t *testing.T) {
	conf := decodeConfigForTest(t, `
dns {
  fakeip {
    enabled: true
    inet4_range: 198.18.0.0/15
    ttl: 90
    store: /tmp/dae-fakeip.db
    direct_upstream: cn
  }
  upstream {
    cn: udp://223.5.5.5:53
  }
  routing {
    request {
      fallback: fakeip
    }
    response {
      fallback: accept
    }
  }
}
`)

	got := conf.Dns.FakeIP
	if !got.Enabled || got.Inet4Range != "198.18.0.0/15" ||
		got.TTL != 90 || got.Store != "/tmp/dae-fakeip.db" ||
		got.DirectUpstream != "cn" {
		t.Fatalf("unexpected fakeip config: %+v", got)
	}
}

func TestDNSFakeIPDefaults(t *testing.T) {
	conf := decodeConfigForTest(t, minimalConfigWithDNS(`
fakeip {
  enabled: true
  direct_upstream: cn
}
`))
	if got := conf.Dns.FakeIP.Inet4Range; got != "198.18.0.0/15" {
		t.Fatalf("inet4_range = %q", got)
	}
	if got := conf.Dns.FakeIP.TTL; got != 60 {
		t.Fatalf("ttl = %d", got)
	}
	if got := conf.Dns.FakeIP.Store; got != "/var/lib/dae/fakeip.db" {
		t.Fatalf("store = %q", got)
	}
}
```

- [ ] **Step 2: Write failing validation tests**

Cover these exact failures:

```go
func TestDNSFakeIPValidation(t *testing.T) {
	tests := []struct {
		name    string
		fakeip  string
		wantErr string
	}{
		{"missing direct upstream", `enabled: true`, "direct_upstream is required"},
		{"invalid prefix", `enabled: true; inet4_range: 2001:db8::/32; direct_upstream: cn`, "must be an IPv4 prefix"},
		{"network or broadcast only", `enabled: true; inet4_range: 198.18.0.0/31; direct_upstream: cn`, "has no allocatable IPv4 addresses"},
		{"zero ttl", `enabled: true; ttl: 0; direct_upstream: cn`, "ttl must be positive"},
		{"unknown upstream", `enabled: true; direct_upstream: missing`, `upstream "missing" not found`},
	}
	// Build a config for every case and assert the error contains wantErr.
}
```

Validation must reject IPv6/mixed prefixes and reserve the subnet network and broadcast addresses.

- [ ] **Step 3: Run the focused tests and verify RED**

```bash
go test ./config ./cmd -run 'Test(DNSFakeIP|DecodeDNSFakeIP|DNSConfigFingerprint)' -count=1
```

Expected: compilation fails because `config.Dns.FakeIP` does not exist.

- [ ] **Step 4: Add the configuration types and defaults**

Add:

```go
type DnsFakeIP struct {
	Enabled        bool   `mapstructure:"enabled" default:"false"`
	Inet4Range     string `mapstructure:"inet4_range" default:"198.18.0.0/15"`
	TTL            int    `mapstructure:"ttl" default:"60"`
	Store          string `mapstructure:"store" default:"/var/lib/dae/fakeip.db"`
	DirectUpstream string `mapstructure:"direct_upstream"`
}

type Dns struct {
	// Existing fields...
	FakeIP DnsFakeIP `mapstructure:"fakeip"`
}
```

Add a config patch that parses `netip.Prefix`, masks it, verifies IPv4, verifies at least two reserved addresses plus one usable address, verifies `TTL > 0`, verifies a non-empty store path, and verifies `DirectUpstream` names an entry in `dns.upstream`.

- [ ] **Step 5: Extend config descriptions, outlines, marshaling, and the DNS fingerprint**

`dnsConfigFingerprint` must serialize all five FakeIP fields. Keep `TestDNSConfigFingerprintCoversAllDnsFields` passing.

Add a separate compatibility helper for the Task 5 reload checks:

```go
func fakeIPStoreIdentity(dns config.Dns) string {
	if !dns.FakeIP.Enabled {
		return "disabled"
	}
	return dns.FakeIP.Inet4Range + "\x00" + dns.FakeIP.Store
}
```

- [ ] **Step 6: Run tests and commit**

```bash
go test ./config ./cmd -run 'Test(DNSFakeIP|DecodeDNSFakeIP|DNSConfigFingerprint)' -count=1
git add config cmd/reload_manager.go cmd/run_shutdown_test.go
git commit -m "feat(config): define IPv4 FakeIP settings"
```

### Task 2: Add `fakeip` as a built-in DNS request outbound

**Files:**
- Modify: `common/consts/dns.go`
- Modify: `component/dns/request_routing.go`
- Modify: `component/dns/dns.go`
- Test: `component/dns/request_routing_test.go`
- Test: `component/dns/routing_program_test.go`
- Test: `component/dns/fallback_contract_test.go`

- [ ] **Step 1: Write failing routing tests**

```go
func TestRequestMatcherSelectsFakeIP(t *testing.T) {
	matcher := buildRequestMatcherForTest(t, `
qname(suffix: cn) -> cn
fallback: fakeip
`)
	got, err := matcher.Match("www.google.com.", dns.TypeA)
	if err != nil {
		t.Fatal(err)
	}
	if got != consts.DnsRequestOutboundIndex_FakeIP {
		t.Fatalf("got %v, want fakeip", got)
	}
}

func TestDNSRequestSelectReturnsNilUpstreamForFakeIP(t *testing.T) {
	routing := newDNSRoutingForTest(t, "fakeip")
	index, upstream, err := routing.RequestSelect(context.Background(), "www.google.com.", dns.TypeA)
	if err != nil {
		t.Fatal(err)
	}
	if index != consts.DnsRequestOutboundIndex_FakeIP || upstream != nil {
		t.Fatalf("selection = (%v, %v)", index, upstream)
	}
}
```

Also assert that internal subscription/node selector splitting rejects `fakeip` where a real upstream is required.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./common/consts ./component/dns -run 'Test.*FakeIP' -count=1
```

Expected: `DnsRequestOutboundIndex_FakeIP` is undefined.

- [ ] **Step 3: Add the reserved value**

Use the next value below `reject`:

```go
const (
	DnsRequestOutboundIndex_FakeIP DnsRequestOutboundIndex = 0xFB
	DnsRequestOutboundIndex_Reject DnsRequestOutboundIndex = 0xFC
	// Existing reserved values...

	DnsRequestOutboundIndex_UserDefinedMax = DnsRequestOutboundIndex_FakeIP - 1
)
```

Return `"fakeip"` from `String()`, parse it in `RequestMatcherBuilder.upstreamToId`, and treat it like `asis`/`reject` in `Dns.RequestSelect`.

- [ ] **Step 4: Run tests and commit**

```bash
go test ./common/consts ./component/dns -count=1
git add common/consts/dns.go component/dns
git commit -m "feat(dns): add fakeip request outbound"
```

### Task 3: Implement the persistent append-only IPv4 mapping store

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `control/fakeip_store.go`
- Create: `control/fakeip_store_test.go`

- [ ] **Step 1: Add the bbolt dependency**

```bash
go get go.etcd.io/bbolt@v1.4.3
```

- [ ] **Step 2: Write failing store tests**

The tests must cover:

```go
func TestFakeIPStoreStableAllocationAndReverseLookup(t *testing.T)
func TestFakeIPStorePersistsAcrossReopen(t *testing.T)
func TestFakeIPStoreConcurrentAllocationIsOneToOne(t *testing.T)
func TestFakeIPStoreNeverAllocatesNetworkOrBroadcast(t *testing.T)
func TestFakeIPStoreReturnsExhaustionWithoutReuse(t *testing.T)
func TestFakeIPStoreWriteFailureDoesNotPublishMapping(t *testing.T)
func TestFakeIPStorePrefixMismatchFailsOpen(t *testing.T)
func TestFakeIPStoreCorruptionIsIsolatedAndRebuilt(t *testing.T)
func TestFakeIPStoreIterationReturnsEveryMapping(t *testing.T)
```

Use a `/29` prefix for deterministic exhaustion. Reopen the same file and assert the mapping is unchanged.

- [ ] **Step 3: Run tests and verify RED**

```bash
go test ./control -run 'TestFakeIPStore' -count=1
```

Expected: store types and constructors are undefined.

- [ ] **Step 4: Implement the store API**

Use these public package-level contracts:

```go
var (
	ErrFakeIPPoolExhausted = errors.New("fakeip IPv4 pool exhausted")
	ErrUnknownFakeIP       = errors.New("unknown fakeip address")
	ErrFakeIPPrefixMismatch = errors.New("fakeip store prefix does not match configuration")
)

type FakeIPStats struct {
	Prefix       netip.Prefix
	Capacity     uint64
	Allocated    uint64
	Remaining    uint64
	Next         netip.Addr
	StorePath    string
	RecoveredDBs uint64
}

type FakeIPStore struct {
	mu       sync.RWMutex
	db       *bbolt.DB
	prefix   netip.Prefix
	path     string
	next     uint32
	first    uint32
	last     uint32
	byDomain map[string]netip.Addr
	byIP     map[netip.Addr]string
	stats    atomic.Pointer[FakeIPStats]
}

func OpenFakeIPStore(path string, prefix netip.Prefix, log *logrus.Logger) (*FakeIPStore, error)
func (s *FakeIPStore) GetOrAllocate(domain string) (netip.Addr, bool, error)
func (s *FakeIPStore) LookupDomain(ip netip.Addr) (string, bool)
func (s *FakeIPStore) Range(fn func(domain string, ip netip.Addr) error) error
func (s *FakeIPStore) Stats() FakeIPStats
func (s *FakeIPStore) Close() error
```

Canonicalize domains with `dns.CanonicalName`. Store schema:

```text
bucket metadata: schema_version, prefix, next
bucket domain_to_ip: canonical domain -> 4-byte IPv4
bucket ip_to_domain: 4-byte IPv4 -> canonical domain
```

Create both mapping rows and advance `next` in one bbolt write transaction. Update in-memory maps only after the transaction commits.

- [ ] **Step 5: Implement narrow corruption recovery**

Do not classify permission, missing-parent, lock-timeout, prefix mismatch, or ordinary write errors as corruption. For bbolt structural/checksum failures:

1. Close the failed handle.
2. Rename the file to `$STORE_PATH.corrupt-YYYYMMDDTHHMMSSZ`.
3. Create a new empty database.
4. Log the isolated path and increment `RecoveredDBs`.

- [ ] **Step 6: Run race tests and commit**

```bash
go test -race ./control -run 'TestFakeIPStore' -count=1
git add go.mod go.sum control/fakeip_store.go control/fakeip_store_test.go
git commit -m "feat(fakeip): add persistent IPv4 mapping store"
```

### Task 4: Synthesize FakeIP DNS responses

**Files:**
- Modify: `control/dns_control.go`
- Modify: `control/dns_cache.go`
- Create: `control/dns_fakeip_test.go`

- [ ] **Step 1: Write failing DNS behavior tests**

Cover these contracts:

```go
func TestFakeIPAQueryReturnsStableSyntheticAddress(t *testing.T)
func TestFakeIPAAAAQueryReturnsNODATA(t *testing.T)
func TestFakeIPNonAddressQueryReturnsNODATA(t *testing.T)
func TestFakeIPPoolExhaustionReturnsSERVFAIL(t *testing.T)
func TestFakeIPWriteFailureReturnsSERVFAIL(t *testing.T)
func TestFakeIPCacheEvictionDoesNotDeletePersistentMapping(t *testing.T)
func TestFakeIPConcurrentQueriesShareOneAllocation(t *testing.T)
```

For NODATA assert `RcodeSuccess` with an empty answer, not NXDOMAIN.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./control -run 'TestFakeIP(A|AAAA|NonAddress|Pool|Write|Cache|Concurrent)' -count=1
```

- [ ] **Step 3: Add FakeIP runtime state**

Add to `DnsControllerOption` and `dnsControllerRuntimeState`:

```go
FakeIPEnabled bool
FakeIPTTL     int
FakeIPStore   *FakeIPStore
```

Add `fakeip` to `responseCacheScope` so synthetic answers never collide with real-upstream cache entries.

- [ ] **Step 4: Add a dedicated synthetic response path before upstream forwarding**

After request routing and before ordinary singleflight forwarding:

```go
if upstreamIndex == consts.DnsRequestOutboundIndex_FakeIP {
	return c.handleFakeIPQuery(dnsMessage, req, responseWriter, responseCacheKey)
}
```

`handleFakeIPQuery` must:

1. Return NODATA for every qtype except A.
2. Allocate or load the canonical domain mapping for A.
3. Build an authoritative-looking successful response with one A RR and configured TTL.
4. Call `UpdateDnsCacheTtlWithKey` so the existing `NewCache` callback computes the business domain bitmap and publishes it to `domain_routing_map`.
5. Return SERVFAIL on allocation or persistence errors.

Do not remove the persistent mapping when the DNS cache entry expires or is evicted.

- [ ] **Step 5: Run tests and commit**

```bash
go test -race ./control -run 'TestFakeIP' -count=1
git add control/dns_control.go control/dns_cache.go control/dns_fakeip_test.go
git commit -m "feat(dns): synthesize persistent FakeIP answers"
```

### Task 5: Initialize, share, and replay FakeIP state across reloads

**Files:**
- Modify: `control/control_plane.go`
- Modify: `control/dns_control.go`
- Modify: `cmd/reload_manager.go`
- Test: `control/control_plane_reload_test.go`
- Test: `cmd/reload_manager_test.go`

- [ ] **Step 1: Write failing reload tests**

```go
func TestFakeIPStoreSharedAcrossCompatibleReload(t *testing.T)
func TestFakeIPMappingsReplayDomainBitmapAfterReload(t *testing.T)
func TestFakeIPTTLAndDirectUpstreamAllowReload(t *testing.T)
func TestFakeIPPrefixChangeRejectsReload(t *testing.T)
func TestFakeIPStorePathChangeRejectsReload(t *testing.T)
func TestFakeIPStoreClosesOnlyAfterFinalControllerClose(t *testing.T)
```

The replay test must clear the fake BPF map, reload with a changed domain matcher, and verify every persistent mapping is republished with the new bitmap.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./control ./cmd -run 'TestFakeIP.*Reload|TestFakeIPStoreShared|TestFakeIPMappingsReplay' -count=1
```

- [ ] **Step 3: Make the store part of the shared DNS controller store**

Add:

```go
type dnsControllerStore struct {
	// Existing fields...
	fakeIPStore *FakeIPStore
}
```

Open it once in `NewDnsController` when enabled. `ReuseForReload` must retain the same store for identical `enabled`, `inet4_range`, and `store`; it updates only TTL and `direct_upstream` in generation-local runtime state.

Close the database inside the existing shared `closeOnce`.

- [ ] **Step 4: Reject incompatible reloads before staged cutover**

Add:

```go
func validateFakeIPReloadCompatibility(oldConf, newConf *config.Config) error {
	if oldConf == nil || newConf == nil {
		return nil
	}
	if fakeIPStoreIdentity(oldConf.Dns) != fakeIPStoreIdentity(newConf.Dns) {
		return fmt.Errorf("dns.fakeip inet4_range/store changes require a full dae restart")
	}
	return nil
}
```

Call it before constructing or publishing the replacement control plane. Enabling or disabling FakeIP also changes store identity and therefore requires a full restart in v1.

- [ ] **Step 5: Replay every persistent mapping**

After `clearReloadDomainRoutingMap`, iterate `FakeIPStore.Range`, recompute `routingMatcher.domainMatcher.MatchDomainBitmap(domain)`, and publish each IP/bitmap pair through a dedicated `controlPlaneCore.UpdateDomainRoutingForAddr` helper.

This replay must not depend on the ordinary DNS cache.

- [ ] **Step 6: Run tests and commit**

```bash
go test -race ./control ./cmd -run 'TestFakeIP|TestDNSConfigFingerprint' -count=1
git add control/control_plane.go control/dns_control.go cmd/reload_manager.go control/*reload_test.go cmd/*reload_test.go
git commit -m "feat(fakeip): preserve mappings across reload"
```

### Task 6: Force FakeIP destinations into userspace without changing routing

**Files:**
- Modify: `control/kern/tproxy.c`
- Modify: `control/bpf_utils.go`
- Modify: `control/bpf_stub.go`
- Modify: `control/kern/tests/bpf_test.c`
- Regenerate: `control/bpf_bpfel.go`
- Regenerate: `control/bpf_bpfeb.go`

- [ ] **Step 1: Write failing BPF tests**

Add cases for:

```text
TCP FakeIP + routing direct  -> redirect, handoff outbound remains direct
TCP FakeIP + routing proxy   -> redirect, handoff outbound remains proxy
TCP FakeIP + routing block   -> drop
UDP FakeIP + routing direct  -> redirect, handoff outbound remains direct
UDP FakeIP + routing proxy   -> redirect, handoff outbound remains proxy
non-FakeIP + routing direct  -> pass through unchanged
disabled FakeIP              -> existing behavior unchanged
```

- [ ] **Step 2: Run BPF tests and verify RED**

```bash
make ebpf-test
```

- [ ] **Step 3: Extend `struct dae_param`**

Append aligned fields:

```c
__u32 fakeip_v4_network;
__u32 fakeip_v4_mask;
__u8 fakeip_enabled;
__u8 fakeip_padding[3];
```

Use network-byte-order values consistently with `pkt->tuples.five.dip.u6_addr32[3]`.

- [ ] **Step 4: Add the address predicate**

```c
static __always_inline bool is_fakeip_v4_destination(const struct tuples_key *five)
{
	if (!PARAM.fakeip_enabled)
		return false;
	__be32 dst = five->dip.u6_addr32[3];
	return (dst & PARAM.fakeip_v4_mask) == PARAM.fakeip_v4_network;
}
```

- [ ] **Step 5: Change only the interception decision**

For new and cached TCP/UDP routing results:

- `OUTBOUND_BLOCK` still drops.
- FakeIP destinations call `redirect_lan_packet_to_control_plane` even when outbound is direct.
- The original `outbound`, `mark`, `must`, and DSCP are copied unchanged into conn state and handoff.
- Non-FakeIP behavior is byte-for-byte equivalent.

Do not assign `OUTBOUND_CONTROL_PLANE_ROUTING` for FakeIP.

- [ ] **Step 6: Populate BPF constants and regenerate objects**

Extend the constants struct in `control/bpf_utils.go` and the stub type. Convert the masked prefix into network/mask values once during control-plane construction.

```bash
make ebpf
make ebpf-test
go test ./control -run 'Test.*Bpf|TestLanIngress' -count=1
```

- [ ] **Step 7: Commit**

```bash
git add control/kern/tproxy.c control/kern/tests control/bpf_utils.go control/bpf_stub.go control/bpf_bpfel.go control/bpf_bpfeb.go
git commit -m "feat(ebpf): intercept IPv4 FakeIP destinations"
```

### Task 7: Restore authoritative domains without rerouting proxy traffic

**Files:**
- Modify: `control/dial.go`
- Modify: `control/tcp.go`
- Create: `control/tcp_fakeip_test.go`

- [ ] **Step 1: Write failing TCP tests**

```go
func TestTCPFakeIPProxyUsesDomainAndPreservesOutbound(t *testing.T)
func TestTCPFakeIPSkipsSniffing(t *testing.T)
func TestTCPUnknownFakeIPIsRejected(t *testing.T)
func TestTCPFakeIPKernelMissRoutesExactlyOnceInUserspace(t *testing.T)
```

The primary assertion is:

```go
if gotRouteCalls != 0 {
	t.Fatalf("business routing called %d times after eBPF result", gotRouteCalls)
}
if gotDialTarget != "www.google.com:443" {
	t.Fatalf("dial target = %q", gotDialTarget)
}
if gotOutbound != proxyFailoverIndex {
	t.Fatalf("outbound changed to %v", gotOutbound)
}
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./control -run 'TestTCPFakeIP' -count=1
```

- [ ] **Step 3: Add an authoritative-domain dial flag**

```go
type proxyDialParam struct {
	// Existing fields...
	AuthoritativeDomain bool
}
```

When true, `chooseProxyDialer` constructs `domain:port` directly, sets `dialIp=false`, and never changes `outboundIndex` to `OutboundControlPlaneRouting`. Existing sniffed-domain behavior remains unchanged.

- [ ] **Step 4: Reverse FakeIP before sniffing**

In `handleConn`, immediately after retrieving the routing result:

```go
domain, isFakeIP, err := c.lookupFakeIPDestination(dst.Addr())
if err != nil {
	return err
}
```

For a known FakeIP:

- Skip TCP sniffing.
- Preserve the eBPF outbound, mark, MAC, process, and DSCP.
- Set `Domain` and `AuthoritativeDomain`.

For an address inside the configured prefix with no mapping, reject before dialing and emit a rate-limited warning.

If the eBPF routing tuple is genuinely missing, reverse the domain first and let the existing userspace fallback call business routing once.

- [ ] **Step 5: Run tests and commit**

```bash
go test -race ./control -run 'TestTCPFakeIP|TestChooseProxyDialer' -count=1
git add control/dial.go control/tcp.go control/tcp_fakeip_test.go
git commit -m "feat(fakeip): dial proxy TCP by authoritative domain"
```

### Task 8: Resolve direct FakeIP traffic through `direct_upstream`

**Files:**
- Modify: `component/dns/dns.go`
- Modify: `control/dns_control.go`
- Modify: `control/tcp.go`
- Test: `component/dns/dns_test.go`
- Test: `control/dns_fakeip_test.go`
- Test: `control/tcp_fakeip_test.go`

This task implements a correctness fallback, not the desired steady-state
direct architecture. Known direct domains must use real DNS and remain on the
existing eBPF kernel fast path. Per-flow counters and diagnostic logs for this
path are deferred to the post-v1 optimization work recorded in the Spec.

- [ ] **Step 1: Write failing named-upstream and direct tests**

```go
func TestDNSLookupNamedUpstream(t *testing.T)
func TestResolveAWithNamedUpstreamBypassesDNSRequestRouting(t *testing.T)
func TestTCPFakeIPDirectResolvesWithConfiguredUpstream(t *testing.T)
func TestTCPFakeIPDirectResolutionFailureDoesNotFallback(t *testing.T)
func TestRealIPDirectTrafficRetainsKernelFastPath(t *testing.T)
```

The bypass test must configure DNS request fallback `fakeip`, then call the direct resolver and prove only the named `cn` upstream received the A query.

- [ ] **Step 2: Expose named upstream selection**

Retain `upstreamName2Id` in `component/dns.Dns` and add:

```go
func (s *Dns) GetUpstreamByName(ctx context.Context, name string) (*Upstream, error)
```

- [ ] **Step 3: Add a direct resolver API**

```go
func (c *DnsController) ResolveAWithUpstream(
	ctx context.Context,
	domain string,
	upstreamName string,
	req *udpRequest,
) ([]netip.Addr, error)
```

It must:

1. Build an A query.
2. Obtain the named upstream directly.
3. Reuse the existing DNS forwarder and `BestDialerChooser`.
4. Bypass `dns.routing.request`, response cache selection, and FakeIP allocation.
5. Return only IPv4 A answers.
6. Return an error on timeout, NODATA, or upstream failure; do not try another upstream.

- [ ] **Step 4: Apply direct resolution after business routing**

For `direct` or `must_direct`, resolve the authoritative domain, replace only `Dest.Addr()` with the selected real IPv4 address, clear the domain before the direct dial, and preserve port, mark, and routing result.

- [ ] **Step 5: Run tests and commit**

```bash
go test -race ./component/dns ./control -run 'Test(DNSLookupNamedUpstream|ResolveAWithNamedUpstream|TCPFakeIPDirect)' -count=1
git add component/dns/dns.go component/dns/*test.go control/dns_control.go control/dns_fakeip_test.go control/tcp.go control/tcp_fakeip_test.go
git commit -m "feat(fakeip): resolve direct TCP through selected DNS"
```

### Task 9: Support UDP and QUIC FakeIP flows

**Files:**
- Modify: `control/udp.go`
- Modify: `control/udp_flow.go`
- Create: `control/udp_fakeip_test.go`

- [ ] **Step 1: Write failing UDP tests**

```go
func TestUDPFakeIPProxyUsesDomainTarget(t *testing.T)
func TestUDPFakeIPDirectUsesResolvedIPv4Target(t *testing.T)
func TestUDPFakeIPSkipsQUICSniffing(t *testing.T)
func TestUDPFakeIPForcesDestinationAffineEndpoint(t *testing.T)
func TestUDPFakeIPUnknownMappingIsRejected(t *testing.T)
func TestUDPFakeIPDistinctDomainsDoNotShareEndpoint(t *testing.T)
```

For proxy UDP, assert `DialOption.Target == "www.google.com:443"` instead of the synthetic IP. For direct UDP, assert target is the A answer from `direct_upstream`.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./control -run 'TestUDPFakeIP' -count=1
```

- [ ] **Step 3: Reverse before endpoint lookup and sniffing**

At the start of `handlePkt`, classify `realDst.Addr()`:

- Known FakeIP: set authoritative domain, skip QUIC sniffing, and force symmetric/destination-affine endpoint keys.
- Unknown address inside the prefix: reject and rate-limit the warning.
- Non-FakeIP: retain current behavior.

Because v1 never reuses FakeIP addresses, the synthetic destination in `UdpEndpointKey.Dst` is sufficient mapping identity.

- [ ] **Step 4: Dial the correct target**

Set `AuthoritativeDomain` on `proxyDialParam`. For proxy outbounds use `res.DialTarget`; remove the current fixed-IP override only for FakeIP flows. For direct outbounds, resolve once through `direct_upstream` and use the real IPv4 address as `DialOption.Target`.

Track conn-state tuples against the original synthetic destination so cleanup still matches the datapath tuple.

- [ ] **Step 5: Run UDP regression tests and commit**

```bash
go test -race ./control -run 'Test(UDPFakeIP|UdpFlowDecision|UDPDirect|Quic)' -count=1
git add control/udp.go control/udp_flow.go control/udp_fakeip_test.go
git commit -m "feat(fakeip): support UDP and QUIC forwarding"
```

### Task 10: Add failure isolation and observability

**Files:**
- Modify: `control/fakeip_store.go`
- Modify: `control/control_plane.go`
- Modify: `control/tcp.go`
- Modify: `control/udp.go`
- Test: `control/fakeip_store_test.go`
- Test: `control/fakeip_observability_test.go`

- [ ] **Step 1: Write failing observability tests**

```go
func TestFakeIPStatsReportCapacityAndUsage(t *testing.T)
func TestFakeIPStartupLogIncludesStoreAndUsage(t *testing.T)
func TestUnknownFakeIPWarningIsRateLimited(t *testing.T)
func TestFakeIPExhaustionLogIncludesCapacity(t *testing.T)
```

- [ ] **Step 2: Add a snapshot API and structured logs**

```go
func (c *ControlPlane) FakeIPStats() (FakeIPStats, bool)
```

Emit structured startup/reload status fields:

```text
prefix, store, allocated, capacity, remaining, next, recovered_dbs
```

Log allocation failures, exhaustion, corruption isolation, and unknown reverse lookups with stable event names. Reuse an atomic timestamp limiter; do not log every rejected packet.

- [ ] **Step 3: Run tests and commit**

```bash
go test -race ./control -run 'TestFakeIP(Stats|Startup|Exhaustion)|TestUnknownFakeIP' -count=1
git add control/fakeip_store.go control/control_plane.go control/tcp.go control/udp.go control/*observability_test.go
git commit -m "feat(fakeip): add status and failure diagnostics"
```

### Task 11: Document configuration and operational constraints

**Files:**
- Modify: `docs/configuration/dns.md`
- Modify: `example.dae`
- Modify: `docs/superpowers/specs/2026-06-14-native-ipv4-fakeip-design.md` only if implementation discoveries require a factual correction

- [ ] **Step 1: Document the exact first-version behavior**

Include:

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

  routing {
    request {
      subnode() -> cn
      qname(suffix: home.arpa, suffix: local) -> localdns
      qname(suffix: cn)
        && qname(geosite:cn) -> cn
      fallback: fakeip
    }
  }
}
```

State explicitly:

- DNS request routing and business routing remain independent.
- A uses FakeIP; AAAA and other qtypes routed to FakeIP return NODATA.
- Proxy traffic sends the domain to the remote endpoint.
- Ordinary direct traffic uses real DNS and retains the eBPF kernel fast path.
- `FakeIP + direct` uses `direct_upstream` only as a first-version safety
  fallback and is a tracked performance issue, not the final architecture.
- No automatic allocation reuse exists in v1.
- Changing enabled state, CIDR, or store path requires restart.
- TTL and `direct_upstream` are reloadable.
- `auto` DNS routing, IPv6 FakeIP, reclamation, and inspection commands are future work.
- Eliminating or minimizing `FakeIP + direct` is mandatory future work; broad
  userspace forwarding of direct traffic is not acceptable.

- [ ] **Step 2: Verify docs and commit**

```bash
rg -n 'fakeip|direct_upstream|198\\.18\\.0\\.0/15|NODATA' docs/configuration/dns.md example.dae
git diff --check
git add docs/configuration/dns.md example.dae
git commit -m "docs: explain native IPv4 FakeIP"
```

### Task 12: Run full source verification

**Files:**
- Verify all modified source and generated files.

- [ ] **Step 1: Format and regenerate**

```bash
gofmt -w \
  common/consts/dns.go \
  component/dns \
  config \
  cmd/reload_manager.go \
  control
make ebpf
```

- [ ] **Step 2: Run focused race tests**

```bash
go test -race ./component/dns ./config ./cmd ./control -run 'FakeIP' -count=1
```

- [ ] **Step 3: Run package tests**

```bash
go test ./common/... ./component/... ./config/... ./cmd/... ./control/... -count=1
```

- [ ] **Step 4: Run BPF tests**

```bash
make ebpf-test
```

- [ ] **Step 5: Run repository checks**

```bash
go vet ./...
git diff --check
git status --short
```

Expected: all tests pass; only intended FakeIP files are modified. Do not add or alter the pre-existing untracked `component/outbound/failover_traffic_flow_test.go` or `tests/`.

- [ ] **Step 6: Commit verification-only fixes, if any**

If formatting, generation, or verification changes a file, return to the task
that owns that file and commit it with that task's explicit file list. Do not
use a blanket `git add`, and do not include the pre-existing untracked files.

### Task 13: Build and perform isolated Linux acceptance

**Files:**
- Build artifact only; no repository file changes required.

- [ ] **Step 1: Build the Linux binary**

```bash
make dae
./dae version
```

- [ ] **Step 2: Run an isolated namespace acceptance test**

Create a temporary network namespace topology and verify:

```text
DNS A google test domain -> synthetic IPv4
DNS AAAA same domain     -> NODATA
TCP synthetic IP        -> selected proxy receives domain target
UDP synthetic IP        -> selected proxy receives domain target
direct rule             -> configured direct upstream receives A query
unknown synthetic IP    -> no packet reaches WAN
restart                 -> same domain receives same synthetic IPv4
```

Capture command output in the implementation session. Do not treat unit tests alone as proof that the transparent datapath works.

- [ ] **Step 3: Confirm no FakeIP leakage**

Use packet capture on the namespace WAN veth and assert no packet has a destination inside the configured FakeIP CIDR.

### Task 14: Deploy safely to `vm-ubuntu-agent` and verify the live gateway

**Files:**
- Live gateway:
  - `/usr/bin/dae` or the currently installed dae binary path
  - `/etc/dae/config.dae`
  - `/var/lib/dae/fakeip.db`
- Repository operations docs only if the final live state differs from existing documentation.

- [ ] **Step 1: Inspect the live source of truth**

Use `$dae-gateway-operations` and one approved SSH session to record:

```bash
systemctl status dae --no-pager
dae version
sudo dae validate -c /etc/dae/config.dae
sudo sed -n '/^dns[[:space:]]*{/,/^}/p' /etc/dae/config.dae
sudo ss -lntup | grep -E ':53[[:space:]]'
```

- [ ] **Step 2: Back up before any live change**

Create timestamped backups of the installed binary and `/etc/dae/config.dae`. Never copy the sanitized repository configuration over production.

- [ ] **Step 3: Install and validate the binary before enabling FakeIP**

Replace the binary atomically, validate the existing production configuration, restart dae, and verify current real-IP DNS behavior remains healthy.

Rollback automatically if validation or startup fails.

- [ ] **Step 4: Apply the smallest live configuration edit**

Add `dns.fakeip`, retain the current `cn`, local, and subscription-node DNS rules, and change only the intended public-domain fallback to `fakeip`.

```bash
sudo dae validate -c /etc/dae/config.dae
sudo systemctl restart dae
```

Because FakeIP enablement is a restart-only change in v1, do not use reload for this step.

- [ ] **Step 5: Verify DNS and datapath from the real client path**

Verify through DAE on port 53:

```bash
dig @127.0.0.1 www.google.com A
dig @127.0.0.1 www.google.com AAAA
dig @127.0.0.1 www.baidu.com A
dig @127.0.0.1 ikuai.home.arpa A
```

Then verify from a LAN client:

- Google/GitHub/YouTube open through the expected proxy/failover group.
- China and local domains keep real addresses and direct behavior.
- A forced primary-node failover does not prevent new public-domain DNS answers.
- Direct-exception domains query `cn` and connect directly.
- TCP and QUIC both work.

- [ ] **Step 6: Verify persistence, reload, and leak prevention**

Record a domain's FakeIP, restart dae, and confirm the same address returns. Perform a TTL-only reload and confirm mappings survive. Capture WAN traffic and confirm no `198.18.0.0/15` destination leaves the gateway.

- [ ] **Step 7: Record final live state**

Update operational documentation only with verified facts: installed version, active DNS ownership, FakeIP CIDR/store, direct upstream, and rollback location.

## Final Acceptance Criteria

- [ ] Public domains selected by DNS request routing receive stable IPv4 FakeIP answers without foreign DNS.
- [ ] China, local, proxy-node, and explicitly excluded domains continue using their selected real upstreams.
- [ ] AAAA and non-A queries selected for FakeIP return NODATA.
- [ ] eBPF performs business routing once and preserves the selected outbound into userspace.
- [ ] Existing real-IP direct traffic remains on the eBPF kernel fast path and does not enter userspace because FakeIP is enabled.
- [ ] Proxy TCP/UDP sends the original domain to the selected proxy without local foreign resolution.
- [ ] The exceptional `FakeIP + direct/must_direct` path resolves only through `direct_upstream`; flow counting is deferred to post-v1 optimization work.
- [ ] Unknown FakeIP addresses never reach the WAN.
- [ ] Mappings survive reload and restart; DNS cache eviction does not reclaim them.
- [ ] Pool exhaustion and persistence failures return SERVFAIL without publishing partial mappings.
- [ ] CIDR/store/enable changes are rejected on reload; TTL/direct-upstream changes reload safely.
- [ ] Existing non-FakeIP DNS, TCP, UDP, QUIC, failover, and reload tests remain green.
