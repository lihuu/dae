/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@daeuniverse.org>
 */

package control

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	componentdns "github.com/daeuniverse/dae/component/dns"
	"github.com/daeuniverse/dae/config"
	dnsmessage "github.com/miekg/dns"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// newFakeIPTestController creates a DnsController with FakeIP routing (fallback=fakeip)
// and a real FakeIPStore backed by a temp BoltDB file.
// The caller can override any DnsControllerOption field via the apply callback.
func newFakeIPTestController(t *testing.T, prefix string, ttl int, apply func(*DnsControllerOption)) *DnsController {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "fakeip.db")
	pfx := netip.MustParsePrefix(prefix)
	store, err := OpenFakeIPStore(dbPath, pfx, logrus.New())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

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

	opt := &DnsControllerOption{
		Log:              logrus.New(),
		LifecycleContext: context.Background(),
		FakeIPEnabled:    true,
		FakeIPTTL:        ttl,
		FakeIPStore:      store,
		CacheAccessCallback: func(*DnsCache) error {
			return nil
		},
		CacheRemoveCallback: func(*DnsCache) error {
			return nil
		},
		NewCache: func(fqdn string, answers, ns, extra []dnsmessage.RR, deadline, originalDeadline time.Time) (*DnsCache, error) {
			return &DnsCache{
				Answer:           answers,
				NS:               ns,
				Extra:            extra,
				Deadline:         deadline,
				OriginalDeadline: originalDeadline,
			}, nil
		},
	}
	if apply != nil {
		apply(opt)
	}

	ctrl, err := NewDnsController(routing, opt)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctrl.Close() })
	return ctrl
}

// doFakeIPQuery sends a DNS query of the given qtype to the controller
// and returns the captured response.
func doFakeIPQuery(t *testing.T, ctrl *DnsController, domain string, qtype uint16) *dnsmessage.Msg {
	t.Helper()

	msg := new(dnsmessage.Msg)
	msg.SetQuestion(dnsmessage.CanonicalName(domain), qtype)
	msg.RecursionDesired = true

	writer := &captureResponseWriter{}
	err := ctrl.HandleWithResponseWriter_(context.Background(), msg, nil, writer)
	require.NoError(t, err)

	resp := writer.Message()
	require.NotNil(t, resp, "expected a DNS response")
	return resp
}

func TestFakeIPAQueryReturnsStableSyntheticAddress(t *testing.T) {
	ctrl := newFakeIPTestController(t, "198.18.0.0/15", 60, nil)

	resp1 := doFakeIPQuery(t, ctrl, "stable.example.com.", dnsmessage.TypeA)
	require.Equal(t, dnsmessage.RcodeSuccess, resp1.Rcode)
	require.Len(t, resp1.Answer, 1, "expected exactly one A record answer")
	a1, ok := resp1.Answer[0].(*dnsmessage.A)
	require.True(t, ok, "answer should be an A record")
	require.True(t, a1.Hdr.Name == "stable.example.com." || a1.Hdr.Name == "Stable.example.com.", "answer name should match query")

	// Second query for the same domain must return the same IP (stable mapping).
	resp2 := doFakeIPQuery(t, ctrl, "stable.example.com.", dnsmessage.TypeA)
	require.Equal(t, dnsmessage.RcodeSuccess, resp2.Rcode)
	require.Len(t, resp2.Answer, 1)
	a2, ok := resp2.Answer[0].(*dnsmessage.A)
	require.True(t, ok)
	require.Equal(t, a1.A, a2.A, "repeated A queries must return the same synthetic IP")
}

func TestFakeIPAAAAQueryReturnsNODATA(t *testing.T) {
	ctrl := newFakeIPTestController(t, "198.18.0.0/15", 60, nil)

	resp := doFakeIPQuery(t, ctrl, "aaaa.example.com.", dnsmessage.TypeAAAA)

	// NODATA: RcodeSuccess with empty answer section (NOT NXDOMAIN).
	require.Equal(t, dnsmessage.RcodeSuccess, resp.Rcode, "NODATA requires RcodeSuccess, not NXDOMAIN")
	require.Empty(t, resp.Answer, "AAAA query routed to fakeip should return empty answer section")
}

func TestFakeIPNonAddressQueryReturnsNODATA(t *testing.T) {
	ctrl := newFakeIPTestController(t, "198.18.0.0/15", 60, nil)

	resp := doFakeIPQuery(t, ctrl, "mx.example.com.", dnsmessage.TypeMX)

	require.Equal(t, dnsmessage.RcodeSuccess, resp.Rcode, "non-A query routed to fakeip should return RcodeSuccess (NODATA)")
	require.Empty(t, resp.Answer, "MX query routed to fakeip should return empty answer section")
}

func TestFakeIPPoolExhaustionReturnsSERVFAIL(t *testing.T) {
	// /30 prefix: 198.51.100.0/30 → network .0, usable .1 and .2, broadcast .3 → 2 allocatable IPs.
	ctrl := newFakeIPTestController(t, "198.51.100.0/30", 60, nil)

	// Exhaust the pool with two distinct domains.
	resp1 := doFakeIPQuery(t, ctrl, "first.exhaust.example.com.", dnsmessage.TypeA)
	require.Equal(t, dnsmessage.RcodeSuccess, resp1.Rcode)
	require.Len(t, resp1.Answer, 1)

	resp2 := doFakeIPQuery(t, ctrl, "second.exhaust.example.com.", dnsmessage.TypeA)
	require.Equal(t, dnsmessage.RcodeSuccess, resp2.Rcode)
	require.Len(t, resp2.Answer, 1)

	// Third domain should fail with SERVFAIL because the pool is exhausted.
	resp3 := doFakeIPQuery(t, ctrl, "third.exhaust.example.com.", dnsmessage.TypeA)
	require.Equal(t, dnsmessage.RcodeServerFailure, resp3.Rcode,
		"pool exhaustion should return SERVFAIL, got rcode=%d", resp3.Rcode)
}

func TestFakeIPWriteFailureReturnsSERVFAIL(t *testing.T) {
	// Use a NewCache callback that always returns an error to simulate a cache
	// write failure (the store allocation succeeds, but persisting to the DNS
	// cache fails).
	ctrl := newFakeIPTestController(t, "198.18.0.0/15", 60, func(opt *DnsControllerOption) {
		opt.NewCache = func(fqdn string, answers, ns, extra []dnsmessage.RR, deadline, originalDeadline time.Time) (*DnsCache, error) {
			return nil, fmt.Errorf("simulated cache write failure")
		}
	})

	resp := doFakeIPQuery(t, ctrl, "writefail.example.com.", dnsmessage.TypeA)
	require.Equal(t, dnsmessage.RcodeServerFailure, resp.Rcode,
		"cache write failure should yield SERVFAIL, got rcode=%d", resp.Rcode)
}

func TestFakeIPCacheEvictionDoesNotDeletePersistentMapping(t *testing.T) {
	ctrl := newFakeIPTestController(t, "198.18.0.0/15", 60, nil)

	// First query establishes the domain→IP mapping.
	resp1 := doFakeIPQuery(t, ctrl, "persist.example.com.", dnsmessage.TypeA)
	require.Equal(t, dnsmessage.RcodeSuccess, resp1.Rcode)
	require.Len(t, resp1.Answer, 1)
	originalIP := resp1.Answer[0].(*dnsmessage.A).A

	// Simulate DNS cache eviction: remove all cache entries for this domain's base keys.
	// The store mapping (BoltDB-backed) must persist independently of the DNS cache.
	baseKeyA := ctrl.cacheKey("persist.example.com.", dnsmessage.TypeA)
	ctrl.RemoveDnsRespCache(baseKeyA)
	scopeKey := baseKeyA + "|" + ctrl.responseCacheScope(nil, consts.DnsRequestOutboundIndex_FakeIP, nil)
	ctrl.RemoveDnsRespCache(scopeKey)

	// Re-query: the store should still have the mapping, so the same IP must come back.
	resp2 := doFakeIPQuery(t, ctrl, "persist.example.com.", dnsmessage.TypeA)
	require.Equal(t, dnsmessage.RcodeSuccess, resp2.Rcode)
	require.Len(t, resp2.Answer, 1)
	persistedIP := resp2.Answer[0].(*dnsmessage.A).A

	require.Equal(t, originalIP, persistedIP,
		"synthetic IP must persist across DNS cache eviction (store mapping is independent)")
}

func TestFakeIPConcurrentQueriesShareOneAllocation(t *testing.T) {
	ctrl := newFakeIPTestController(t, "198.18.0.0/15", 60, nil)

	const N = 32
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ips  = make([]netip.Addr, 0, N)
		errs = make([]error, 0, N)
	)

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			msg := new(dnsmessage.Msg)
			msg.SetQuestion(dnsmessage.CanonicalName("concurrent.example.com."), dnsmessage.TypeA)
			msg.RecursionDesired = true

			writer := &captureResponseWriter{}
			err := ctrl.HandleWithResponseWriter_(context.Background(), msg, nil, writer)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			resp := writer.Message()
			if resp == nil || len(resp.Answer) == 0 {
				errs = append(errs, fmt.Errorf("nil/empty response"))
				return
			}
			a, ok := resp.Answer[0].(*dnsmessage.A)
			if !ok {
				errs = append(errs, fmt.Errorf("answer is not an A record"))
				return
			}
			ip, ok := netip.AddrFromSlice(a.A)
			if !ok {
				errs = append(errs, fmt.Errorf("invalid IP in answer"))
				return
			}
			ips = append(ips, ip.Unmap())
		}()
	}
	wg.Wait()

	require.Empty(t, errs, "no errors expected from concurrent queries: %v", errs)
	require.Len(t, ips, N)

	// All goroutines should have observed the same synthetic IP (single allocation
	// coalesced by the store's double-checked locking).
	for i := 1; i < len(ips); i++ {
		require.Equal(t, ips[0], ips[i],
			"all concurrent A queries for the same domain must return the same synthetic IP")
	}
}
