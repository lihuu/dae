/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/daeuniverse/dae/common/consts"
	dnsmessage "github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

// handleFakeIPQuery synthesizes a FakeIP DNS response without forwarding to a
// real upstream. For A queries it allocates (or loads) a persistent synthetic
// IPv4 from the FakeIPStore and returns it in an authoritative A record. For
// all other qtypes it returns NODATA (RcodeSuccess with an empty answer
// section). The synthetic response is pushed through the DNS cache so that the
// existing NewCache callback computes the domain bitmap and publishes it to the
// eBPF domain_routing_map.
//
// On store or cache errors, SERVFAIL is returned. The persistent store mapping
// is intentionally NOT removed when the DNS cache entry expires or is evicted.
func (c *DnsController) handleFakeIPQuery(
	dnsMessage *dnsmessage.Msg,
	req *udpRequest,
	responseWriter dnsmessage.ResponseWriter,
	responseCacheKey string,
) error {
	if len(dnsMessage.Question) == 0 {
		return c.sendDnsErrorResponse_(dnsMessage, dnsmessage.RcodeServerFailure, false,
			"FakeIP: no question", req, responseWriter)
	}

	rt := c.runtime()
	q := dnsMessage.Question[0]
	qname := q.Name
	qtype := q.Qtype

	// Non-A queries: return NODATA (RcodeSuccess with empty answer section).
	// The domain exists, but FakeIP only synthesizes A records.
	if qtype != dnsmessage.TypeA {
		c.logFakeIPDNSAnswerNODATA(qname, qtype)
		dnsMessage.Answer = nil
		dnsMessage.Rcode = dnsmessage.RcodeSuccess
		dnsMessage.Response = true
		dnsMessage.Authoritative = true
		dnsMessage.RecursionAvailable = true
		dnsMessage.Truncated = false
		dnsMessage.Compress = true
		if responseWriter != nil {
			return responseWriter.WriteMsg(dnsMessage)
		}
		if req == nil || req.lConn == nil {
			return nil
		}
		data, err := dnsMessage.Pack()
		if err != nil {
			return fmt.Errorf("pack FakeIP NODATA response: %w", err)
		}
		return sendRuntimeTrackedPkt(c.log, data, req.realDst, req.realSrc, req.replySoMark(), req.downloadRecorder())
	}

	// A query: allocate or load the canonical domain->IP mapping from the store.
	if rt == nil || !rt.fakeIPEnabled || rt.fakeIPStore == nil {
		c.logFakeIPDNSAnswerFailed(qname, qtype, "fakeip_not_configured", nil)
		return c.sendDnsErrorResponse_(dnsMessage, dnsmessage.RcodeServerFailure, false,
			"FakeIP: not configured", req, responseWriter)
	}

	ttl := rt.fakeIPTTL
	if ttl <= 0 {
		ttl = 60
	}

	ip, allocated, err := rt.fakeIPStore.GetOrAllocate(qname)
	if err != nil {
		c.logFakeIPDNSAnswerFailed(qname, qtype, "fakeip_store_error", err)
		if c.log != nil {
			c.log.WithFields(logrus.Fields{
				"domain": strings.ToLower(qname),
			}).Warnf("FakeIP store allocation failed: %v", err)
		}
		return c.sendDnsErrorResponse_(dnsMessage, dnsmessage.RcodeServerFailure, false,
			"FakeIP: store allocation failed", req, responseWriter)
	}

	// Build an authoritative-looking successful response with one A RR.
	ip4 := ip.As4()
	answer := &dnsmessage.A{
		Hdr: dnsmessage.RR_Header{
			Name:   qname,
			Rrtype: dnsmessage.TypeA,
			Class:  dnsmessage.ClassINET,
			Ttl:    uint32(ttl),
		},
		A: net.IPv4(ip4[0], ip4[1], ip4[2], ip4[3]),
	}

	dnsMessage.Answer = []dnsmessage.RR{answer}
	dnsMessage.Rcode = dnsmessage.RcodeSuccess
	dnsMessage.Response = true
	dnsMessage.Authoritative = true
	dnsMessage.RecursionAvailable = true
	dnsMessage.Truncated = false
	dnsMessage.Compress = true

	// Push through the DNS cache so the domain bitmap gets published to
	// the eBPF domain_routing_map. Cache write failure -> SERVFAIL.
	if cacheErr := c.UpdateDnsCacheTtlWithKey(
		responseCacheKey, qname, dnsmessage.TypeA,
		dnsMessage.Answer, nil, nil, ttl,
	); cacheErr != nil {
		c.logFakeIPDNSAnswerFailed(qname, qtype, "fakeip_cache_error", cacheErr)
		if c.log != nil {
			c.log.WithFields(logrus.Fields{
				"domain": strings.ToLower(qname),
			}).Warnf("FakeIP: failed to update DNS cache: %v", cacheErr)
		}
		return c.sendDnsErrorResponse_(dnsMessage, dnsmessage.RcodeServerFailure, false,
			"FakeIP: cache update failed", req, responseWriter)
	}

	// Also publish a persistent owner ("fakeip:<addr>") so the bitmap
	// survives DNS cache eviction. Without this, the domain routing entry
	// in eBPF is lost when the cache entry expires (TTL/LRU) and is only
	// restored on the next DNS query or after a reload replay.
	if publisher := rt.fakeIPBitmapPublisher; publisher != nil {
		if pubErr := publisher(qname, ip); pubErr != nil {
			if c.log != nil {
				c.log.WithFields(logrus.Fields{
					"domain": strings.ToLower(qname),
					"addr":   ip.String(),
				}).Warnf("FakeIP: failed to publish persistent bitmap: %v", pubErr)
			}
			// Non-fatal: the cache-keyed owner is still in place.
		}
	}

	c.logFakeIPDNSAnswerOK(qname, ip, ttl, allocated)

	// Send the synthesized response to the client.
	if responseWriter != nil {
		return responseWriter.WriteMsg(dnsMessage)
	}
	if req == nil || req.lConn == nil {
		return nil
	}
	data, err := dnsMessage.Pack()
	if err != nil {
		return fmt.Errorf("pack FakeIP A response: %w", err)
	}
	return sendRuntimeTrackedPkt(c.log, data, req.realDst, req.realSrc, req.replySoMark(), req.downloadRecorder())
}

func (c *DnsController) serveFakeIPWithWriter_(
	dnsMessage *dnsmessage.Msg,
	req *udpRequest,
	responseWriter dnsmessage.ResponseWriter,
	responseCacheKey string,
	upstreamIndex consts.DnsRequestOutboundIndex,
) (bool, error) {
	if upstreamIndex != consts.DnsRequestOutboundIndex_FakeIP {
		return false, nil
	}
	return true, c.handleFakeIPQuery(dnsMessage, req, responseWriter, responseCacheKey)
}

// replayFakeIPMappings re-publishes every persistent FakeIP domain->IP mapping
// to the eBPF domain_routing_map using the supplied matchDomainBitmap function
// to recompute domain bitmaps under the (possibly changed) routing rules of the
// new generation. Each IP/bitmap pair is published through the supplied
// publishFn, which is typically wired to controlPlaneCore.UpdateDomainRoutingForAddr.
//
// This is called after clearReloadDomainRoutingMap wipes the map so that the
// kernel-space routing table is repopulated with the new bitmaps for all
// synthetic FakeIP addresses. Unlike ordinary DNS cache replay, this does not
// depend on the DNS cache — it reads directly from the persistent BoltDB store.
//
// Returns the number of mappings successfully republished, the number that
// failed to publish, and any error from the underlying store iteration.
func (c *DnsController) replayFakeIPMappings(
	matchDomainBitmap func(string) []uint32,
	publishFn func(domain string, addr netip.Addr, domainBitmap []uint32) error,
) (count int, failed int, rangeErr error) {
	if c == nil {
		return 0, 0, nil
	}
	store := c.dnsControllerStore
	if store == nil || store.fakeIPStore == nil {
		return 0, 0, nil
	}
	if matchDomainBitmap == nil || publishFn == nil {
		return 0, 0, nil
	}

	rangeErr = store.fakeIPStore.Range(func(domain string, ip netip.Addr) error {
		bitmap := matchDomainBitmap(domain)
		if err := publishFn(domain, ip, bitmap); err != nil {
			failed++
			if c.log != nil {
				c.log.WithError(err).Warnf("failed to replay FakeIP mapping for %s", domain)
			}
		} else {
			count++
		}
		return nil
	})
	return count, failed, rangeErr
}

// ResolveAWithUpstream resolves a domain's A records using the named upstream
// directly, bypassing DNS request routing, response cache, and FakeIP allocation.
// Returns only IPv4 A answers. Returns an error on timeout, NODATA, or upstream
// failure; does not try another upstream.
func (c *DnsController) ResolveAWithUpstream(
	ctx context.Context,
	domain string,
	upstreamName string,
	req *udpRequest,
) ([]netip.Addr, error) {
	rt := c.runtime()
	if rt == nil {
		return nil, fmt.Errorf("dns controller runtime not available")
	}
	if rt.routing == nil {
		return nil, fmt.Errorf("dns component not available")
	}

	// 1. Obtain the named upstream directly.
	upstream, err := rt.routing.GetUpstreamByName(ctx, upstreamName)
	if err != nil {
		return nil, fmt.Errorf("get upstream %q: %w", upstreamName, err)
	}

	// 2. Get a dialer via the existing chooser.
	if rt.bestDialerChooser == nil {
		return nil, fmt.Errorf("bestDialerChooser not configured")
	}
	snapshot := dnsRequestSnapshotFromUDPRequest(req)
	dialArg, err := rt.chooseBestDnsDialer(ctx, snapshot, upstream)
	if err != nil {
		return nil, fmt.Errorf("choose dialer for upstream %q: %w", upstreamName, err)
	}

	// 3. Create a forwarder for this upstream.
	forwarder, err := newDnsForwarder(upstream, *dialArg, c.log)
	if err != nil {
		return nil, fmt.Errorf("create forwarder for upstream %q: %w", upstreamName, err)
	}
	defer forwarder.Close()

	// 4. Build an A query.
	qname := dnsmessage.CanonicalName(domain)
	msg := new(dnsmessage.Msg)
	msg.RecursionDesired = true
	msg.Question = []dnsmessage.Question{
		{
			Name:   qname,
			Qtype:  dnsmessage.TypeA,
			Qclass: dnsmessage.ClassINET,
		},
	}
	data, err := msg.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack dns query: %w", err)
	}

	// 5. Send the query.
	resp, err := forwarder.ForwardDNS(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("forward dns to upstream %q: %w", upstreamName, err)
	}
	if resp.Rcode != dnsmessage.RcodeSuccess {
		return nil, fmt.Errorf("upstream %q returned rcode %v for %q", upstreamName, resp.Rcode, domain)
	}

	// 6. Extract A records only.
	var addrs []netip.Addr
	for _, ans := range resp.Answer {
		switch r := ans.(type) {
		case *dnsmessage.A:
			addr, ok := netip.AddrFromSlice(r.A[:])
			if ok {
				addrs = append(addrs, addr.Unmap())
			}
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no A records for %q from upstream %q", domain, upstreamName)
	}
	return addrs, nil
}
