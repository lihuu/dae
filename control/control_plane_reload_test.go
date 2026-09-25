/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"context"
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	componentdns "github.com/daeuniverse/dae/component/dns"
	"github.com/daeuniverse/dae/config"
	dnsmessage "github.com/miekg/dns"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// newFakeIPStoreForTest opens a FakeIPStore backed by a temp file.
func newFakeIPStoreForTest(t *testing.T, prefix string) *FakeIPStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fakeip.db")
	pfx := netip.MustParsePrefix(prefix)
	store, err := OpenFakeIPStore(dbPath, pfx, logrus.New())
	require.NoError(t, err)
	return store
}

// newFakeIPRouting builds a dns.Dns with fallback=fakeip suitable for tests.
func newFakeIPRouting(t *testing.T) *componentdns.Dns {
	t.Helper()
	routing, err := componentdns.New(&config.Dns{
		Upstream: []config.KeyableString{"cn:udp://223.5.5.5:53"},
		Routing: config.DnsRouting{
			Request:  config.DnsRequestRouting{Fallback: "fakeip"},
			Response: config.DnsResponseRouting{Fallback: "accept"},
		},
	}, &componentdns.NewOption{
		Logger: logrus.New(),
		UpstreamReadyCallback: func(*componentdns.Upstream) error {
			return nil
		},
	})
	require.NoError(t, err)
	return routing
}

// TestFakeIPStoreSharedAcrossCompatibleReload verifies that ReuseForReload
// preserves the same FakeIPStore instance across reloads when the store
// identity (enabled, inet4_range, store path) is unchanged. Only TTL and
// direct_upstream (generation-local runtime state) should be updated.
func TestFakeIPStoreSharedAcrossCompatibleReload(t *testing.T) {
	store := newFakeIPStoreForTest(t, "198.18.0.0/29")
	defer store.Close()
	routing := newFakeIPRouting(t)

	// Phase 1: Create the initial DNS controller with FakeIP enabled.
	ctrl, err := NewDnsController(routing, &DnsControllerOption{
		Log:                 logrus.New(),
		LifecycleContext:    context.Background(),
		FakeIPEnabled:       true,
		FakeIPTTL:           60,
		FakeIPStore:         store,
		CacheAccessCallback: func(*DnsCache) error { return nil },
		CacheDeleteCallback: func(string, *DnsCache) error { return nil },
	})
	require.NoError(t, err)
	defer ctrl.Close()

	// The shared store should carry the FakeIP store.
	require.NotNil(t, ctrl.dnsControllerStore)
	require.Same(t, store, ctrl.dnsControllerStore.fakeIPStore,
		"NewDnsController must install the FakeIP store into the shared store")

	// Phase 2: Simulate reload with changed TTL (same store identity).
	reused, err := ctrl.ReuseForReload(&DnsControllerOption{
		Log:              logrus.New(),
		LifecycleContext: context.Background(),
		FakeIPEnabled:    true,
		FakeIPTTL:        120, // changed
		// FakeIPStore intentionally omitted — should reuse from shared store.
		CacheAccessCallback: func(*DnsCache) error { return nil },
		CacheDeleteCallback: func(string, *DnsCache) error { return nil },
	}, routing)
	require.NoError(t, err)
	require.NotNil(t, reused)
	require.NotSame(t, ctrl, reused, "ReuseForReload must return a fresh facade")
	require.Same(t, ctrl.dnsControllerStore, reused.dnsControllerStore,
		"reused facade must share the same dnsControllerStore")
	require.Same(t, store, reused.dnsControllerStore.fakeIPStore,
		"the FakeIP store must survive the reload unchanged")

	// Phase 3: Runtime state should reflect the new TTL.
	rt := reused.runtime()
	require.NotNil(t, rt)
	require.True(t, rt.fakeIPEnabled)
	require.Equal(t, 120, rt.fakeIPTTL, "TTL should be updated to the new value")
	require.Same(t, store, rt.fakeIPStore, "runtime should carry the same store")
}

// TestFakeIPMappingsReplayDomainBitmapAfterReload verifies that after reload,
// every persistent FakeIP mapping is republished with the new domain bitmap.
func TestFakeIPMappingsReplayDomainBitmapAfterReload(t *testing.T) {
	store := newFakeIPStoreForTest(t, "198.18.0.0/29")
	defer store.Close()
	routing := newFakeIPRouting(t)

	// Phase 1: Allocate several persistent mappings.
	domains := []string{"alpha.example.com.", "beta.example.com.", "gamma.example.com."}
	allocatedIPs := make(map[string]netip.Addr, len(domains))
	for _, d := range domains {
		ip, _, err := store.GetOrAllocate(d)
		require.NoError(t, err)
		allocatedIPs[d] = ip
	}

	// Phase 2: Construct the initial DNS controller.
	type publishedEntry struct {
		ownerKey     string
		domainBitmap []uint32
		ips          []netip.Addr
	}
	var (
		pubMu        sync.Mutex
		publications []publishedEntry
	)
	recordPublication := func(cache *DnsCache) error {
		pubMu.Lock()
		defer pubMu.Unlock()
		if cache == nil {
			return nil
		}
		var ips []netip.Addr
		for _, ans := range cache.Answer {
			if a, ok := ans.(*dnsmessage.A); ok {
				if addr, ok := netip.AddrFromSlice(a.A); ok {
					ips = append(ips, addr.Unmap())
				}
			}
		}
		bitmap := make([]uint32, len(cache.DomainBitmap))
		copy(bitmap, cache.DomainBitmap)
		publications = append(publications, publishedEntry{
			ownerKey:     cache.RouteOwnerKey,
			domainBitmap: bitmap,
			ips:          ips,
		})
		return nil
	}

	ctrl, err := NewDnsController(routing, &DnsControllerOption{
		Log:                 logrus.New(),
		LifecycleContext:    context.Background(),
		FakeIPEnabled:       true,
		FakeIPTTL:           60,
		FakeIPStore:         store,
		CacheAccessCallback: recordPublication,
		CacheDeleteCallback: func(string, *DnsCache) error { return nil },
	})
	require.NoError(t, err)
	defer ctrl.Close()

	// Phase 3: Simulate reload with a new bitmap function.
	var newBitmapCalls atomic.Int32
	matchBitmap := func(domain string) []uint32 {
		newBitmapCalls.Add(1)
		switch domain {
		case "alpha.example.com.":
			return []uint32{1}
		case "beta.example.com.":
			return []uint32{2}
		case "gamma.example.com.":
			return []uint32{4}
		default:
			return []uint32{0}
		}
	}

	reused, err := ctrl.ReuseForReload(&DnsControllerOption{
		Log:                 logrus.New(),
		LifecycleContext:    context.Background(),
		FakeIPEnabled:       true,
		FakeIPTTL:           60,
		FakeIPStore:         store,
		CacheAccessCallback: recordPublication,
		CacheDeleteCallback: func(string, *DnsCache) error { return nil },
	}, routing)
	require.NoError(t, err)
	require.NotNil(t, reused)

	// Phase 4: Replay persistent mappings through the reused controller.
	pubMu.Lock()
	publications = nil
	pubMu.Unlock()

	publishFn := func(domain string, addr netip.Addr, domainBitmap []uint32) error {
		pubMu.Lock()
		defer pubMu.Unlock()
		bitmap := make([]uint32, len(domainBitmap))
		copy(bitmap, domainBitmap)
		publications = append(publications, publishedEntry{
			ownerKey:     "fakeip:" + domain,
			domainBitmap: bitmap,
			ips:          []netip.Addr{addr},
		})
		return nil
	}
	reused.replayFakeIPMappings(matchBitmap, publishFn)

	replayCalls := newBitmapCalls.Load()

	// Phase 5: Verify every persistent mapping was published with the new bitmap.
	pubMu.Lock()
	got := make([]publishedEntry, len(publications))
	copy(got, publications)
	pubMu.Unlock()

	require.Len(t, got, len(domains), "every persistent mapping should be republished")

	byOwner := make(map[string]publishedEntry, len(got))
	for _, p := range got {
		byOwner[p.ownerKey] = p
	}

	for _, d := range domains {
		expectedBitmap := matchBitmap(d)
		entry, ok := byOwner["fakeip:"+d]
		require.True(t, ok, "expected replay publication for domain %s", d)
		require.Equal(t, expectedBitmap, entry.domainBitmap,
			"replay must recompute the domain bitmap for %s", d)
		require.Len(t, entry.ips, 1, "each FakeIP mapping should carry exactly one IP")
		require.Equal(t, allocatedIPs[d], entry.ips[0],
			"replay must preserve the original domain->IP mapping for %s", d)
	}

	require.EqualValues(t, len(domains), replayCalls,
		"MatchDomainBitmap should be called once per persistent mapping during replay")
}

// TestFakeIPTTLAndDirectUpstreamAllowReload verifies that changing TTL or
// direct_upstream does NOT prevent the store from being shared across reload.
func TestFakeIPTTLAndDirectUpstreamAllowReload(t *testing.T) {
	store := newFakeIPStoreForTest(t, "198.18.0.0/29")
	defer store.Close()
	routing := newFakeIPRouting(t)

	ctrl, err := NewDnsController(routing, &DnsControllerOption{
		Log:                 logrus.New(),
		LifecycleContext:    context.Background(),
		FakeIPEnabled:       true,
		FakeIPTTL:           60,
		FakeIPStore:         store,
		CacheAccessCallback: func(*DnsCache) error { return nil },
		CacheDeleteCallback: func(string, *DnsCache) error { return nil },
	})
	require.NoError(t, err)
	defer ctrl.Close()

	// Reload with changed TTL.
	reused, err := ctrl.ReuseForReload(&DnsControllerOption{
		Log:                 logrus.New(),
		LifecycleContext:    context.Background(),
		FakeIPEnabled:       true,
		FakeIPTTL:           300, // changed
		CacheAccessCallback: func(*DnsCache) error { return nil },
		CacheDeleteCallback: func(string, *DnsCache) error { return nil },
	}, routing)
	require.NoError(t, err)
	require.NotNil(t, reused)
	require.Same(t, store, reused.dnsControllerStore.fakeIPStore,
		"TTL change must not cause a new store to be created")
	require.Equal(t, 300, reused.runtime().fakeIPTTL,
		"new generation should observe updated TTL")
}

// TestFakeIPStoreClosesOnlyAfterFinalControllerClose verifies that FakeIPStore.Close
// is called exactly once when the shared dnsControllerStore's closeOnce fires.
func TestFakeIPStoreClosesOnlyAfterFinalControllerClose(t *testing.T) {
	store := newFakeIPStoreForTest(t, "198.18.0.0/29")
	routing := newFakeIPRouting(t)

	ctrl, err := NewDnsController(routing, &DnsControllerOption{
		Log:                 logrus.New(),
		LifecycleContext:    context.Background(),
		FakeIPEnabled:       true,
		FakeIPTTL:           60,
		FakeIPStore:         store,
		CacheAccessCallback: func(*DnsCache) error { return nil },
		CacheDeleteCallback: func(string, *DnsCache) error { return nil },
	})
	require.NoError(t, err)

	reused, err := ctrl.ReuseForReload(&DnsControllerOption{
		Log:                 logrus.New(),
		LifecycleContext:    context.Background(),
		FakeIPEnabled:       true,
		FakeIPTTL:           60,
		CacheAccessCallback: func(*DnsCache) error { return nil },
		CacheDeleteCallback: func(string, *DnsCache) error { return nil },
	}, routing)
	require.NoError(t, err)
	require.NotNil(t, reused)

	require.NoError(t, ctrl.Close())

	_, _, allocErr := store.GetOrAllocate("should.fail.")
	require.Error(t, allocErr,
		"allocation must fail after the shared store is closed via closeOnce")

	require.NoError(t, reused.Close())
}

// TestReplayFakeIPMappingsCountsFailures verifies that replayFakeIPMappings
// reports per-entry publish failures via the failed counter.
func TestReplayFakeIPMappingsCountsFailures(t *testing.T) {
	store := newFakeIPStoreForTest(t, "198.18.0.0/29")
	defer store.Close()
	routing := newFakeIPRouting(t)

	domains := []string{"ok1.example.com.", "boom.example.com.", "ok2.example.com."}
	for _, d := range domains {
		_, _, err := store.GetOrAllocate(d)
		require.NoError(t, err)
	}

	ctrl, err := NewDnsController(routing, &DnsControllerOption{
		Log:                 logrus.New(),
		LifecycleContext:    context.Background(),
		FakeIPEnabled:       true,
		FakeIPTTL:           60,
		FakeIPStore:         store,
		CacheAccessCallback: func(*DnsCache) error { return nil },
		CacheDeleteCallback: func(string, *DnsCache) error { return nil },
	})
	require.NoError(t, err)
	defer ctrl.Close()

	matchBitmap := func(string) []uint32 { return []uint32{1} }
	publishFn := func(domain string, addr netip.Addr, domainBitmap []uint32) error {
		if domain == "boom.example.com." {
			return errReplayPublishFail
		}
		return nil
	}

	count, failed, rangeErr := ctrl.replayFakeIPMappings(matchBitmap, publishFn)
	require.NoError(t, rangeErr, "store iteration must not error")
	require.Equal(t, 2, count, "two domains should publish successfully")
	require.Equal(t, 1, failed, "one domain should be counted as failed")
}

var errReplayPublishFail = &replayPublishErr{}

type replayPublishErr struct{}

func (e *replayPublishErr) Error() string { return "simulated publish failure" }
