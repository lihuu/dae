/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package dns

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/daeuniverse/dae/common"
	"github.com/daeuniverse/dae/common/assets"
	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/common/netutils"
	"github.com/daeuniverse/dae/component/routing"
	"github.com/daeuniverse/dae/config"
	dnsmessage "github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

var ErrBadUpstreamFormat = fmt.Errorf("bad upstream format")

type Dns struct {
	log            *logrus.Logger
	upstream       []*UpstreamResolver
	upstream2Index sync.Map
	reqMatcher     *RequestMatcher
	respMatcher    *ResponseMatcher
	// nameToIndex maps upstream tag names to their indices for named lookup.
	nameToIndex map[string]uint8
}

type NewOption struct {
	Logger                  *logrus.Logger
	LocationFinder          *assets.LocationFinder
	DatReaderOptimizer      *routing.DatReaderOptimizer
	UpstreamReadyCallback   func(dnsUpstream *Upstream) (err error)
	UpstreamResolverNetwork string
	UpstreamHostResolver    func(ctx context.Context, host string, network string) (*netutils.Ip46, error, error)
	// Stats, when non-nil, receives per-substage construction durations from
	// New. The seven fields decompose dns_controller_build into the substages
	// observed inside dns.New so an operator can attribute startup time inside
	// the otherwise-opaque dns_controller_build stage.
	Stats *BuildStats
	// PrebuiltRequestMatcher, when non-nil, skips the entire request-side
	// build inside New: program normalize, builder construction, and
	// AhocorasickSlimtrie compile. The caller is responsible for ensuring the
	// supplied matcher was built from the same dnsCfg.Routing.Request.Rules
	// and the same dnsCfg.Upstream slice (in the same order) so the matcher's
	// outbound-index encoding aligns with Dns.upstream populated here.
	//
	// When the caller already built a daedns.Router for dialer-side DNS
	// resolution, daedns.Router.RequestMatcher() returns exactly this matcher.
	// Reusing it eliminates the second AhocorasickSlimtrie.Build pass that
	// otherwise dominates dns_controller_build_ms.
	PrebuiltRequestMatcher *RequestMatcher
}

// BuildStats captures per-substage durations from New. The sum of the seven
// fields is expected to be ≈ the parent dns_controller_build_ms; any gap
// surfaces as dns_controller_unattributed_ms in the rules_load summary.
type BuildStats struct {
	// UpstreamInit covers the upstream URL-parse loop at the top of New.
	UpstreamInit time.Duration
	// RequestProgramNormalize covers NewNormalizedRequestRoutingProgram (the
	// DatReaderOptimizer geosite/geoip expansion for DNS request rules).
	RequestProgramNormalize time.Duration
	// RequestMatcherLower covers NewRequestMatcherBuilderFromProgram (program
	// → simulatedDomainSet + rule list; cheap glue).
	RequestMatcherLower time.Duration
	// RequestMatcherCompile covers RequestMatcherBuilder.Build (the heavy AC
	// slimtrie compile over DNS request rules).
	RequestMatcherCompile time.Duration
	// ResponseProgramNormalize covers routing.NewNormalizedProgram for the
	// response routing program.
	ResponseProgramNormalize time.Duration
	// ResponseMatcherLower covers NewResponseMatcherBuilderFromProgram.
	ResponseMatcherLower time.Duration
	// ResponseMatcherCompile covers ResponseMatcherBuilder.Build (AC slimtrie
	// compile over DNS response rules + IpSet aggregation).
	ResponseMatcherCompile time.Duration
}

func New(dns *config.Dns, opt *NewOption) (s *Dns, err error) {
	s = &Dns{
		log: opt.Logger,
	}

	var stats *BuildStats
	if opt != nil {
		stats = opt.Stats
	}
	stamp := func(field func(*BuildStats) *time.Duration, start time.Time) {
		if stats == nil {
			return
		}
		*field(stats) = time.Since(start)
	}

	upstreamStart := time.Now()
	s.upstream2Index.Store((*Upstream)(nil), int(consts.DnsRequestOutboundIndex_AsIs))
	// Parse upstream.
	upstreamName2Id := map[string]uint8{}
	for i, upstreamRaw := range dns.Upstream {
		if i >= int(consts.DnsRequestOutboundIndex_UserDefinedMax) ||
			i >= int(consts.DnsResponseOutboundIndex_UserDefinedMax) {
			return nil, fmt.Errorf("too many upstreams")
		}

		tag, link := common.GetTagFromLinkLikePlaintext(string(upstreamRaw))
		if tag == "" {
			return nil, fmt.Errorf("%w: '%v' has no tag", ErrBadUpstreamFormat, upstreamRaw)
		}
		var u *url.URL
		u, err = url.Parse(link)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadUpstreamFormat, err)
		}
		r := &UpstreamResolver{
			Raw:         u,
			Network:     opt.UpstreamResolverNetwork,
			ResolveIp46: opt.UpstreamHostResolver,
			FinishInitCallback: func(i int) func(raw *url.URL, upstream *Upstream) (err error) {
				return func(raw *url.URL, upstream *Upstream) (err error) {
					if opt.UpstreamReadyCallback != nil { // Redundant comparison 'opt != nil' removed
						if err = opt.UpstreamReadyCallback(upstream); err != nil {
							return err
						}
					}

					s.upstream2Index.Store(upstream, i)
					return nil
				}
			}(i),
		}
		upstreamName2Id[tag] = uint8(len(s.upstream))
		s.upstream = append(s.upstream, r)
	}
	s.nameToIndex = upstreamName2Id
	stamp(func(s *BuildStats) *time.Duration { return &s.UpstreamInit }, upstreamStart)

	datReaderOptimizer := datReaderOptimizerForDNS(opt)
	if opt != nil && opt.PrebuiltRequestMatcher != nil {
		// Reuse path: caller already built the request matcher (e.g. the
		// daedns.Router built upstream of the control plane). Skip request
		// program normalize + builder construction + AC slimtrie compile. The
		// three request-side BuildStats fields remain zero — that zero is the
		// signal an operator wants: "this work didn't happen because we
		// reused".
		s.reqMatcher = opt.PrebuiltRequestMatcher
	} else {
		reqProgStart := time.Now()
		requestProgram, err := NewNormalizedRequestRoutingProgram(dns.Routing.Request.Rules, dns.Routing.Request.Fallback,
			datReaderOptimizer,
			&routing.MergeAndSortRulesOptimizer{},
			&routing.DeduplicateParamsOptimizer{},
		)
		if err != nil {
			return nil, err
		}
		stamp(func(s *BuildStats) *time.Duration { return &s.RequestProgramNormalize }, reqProgStart)

		// Parse request routing.
		reqLowerStart := time.Now()
		reqMatcherBuilder, err := NewRequestMatcherBuilderFromProgram(opt.Logger, requestProgram, upstreamName2Id)
		if err != nil {
			return nil, fmt.Errorf("failed to build DNS request routing: %w", err)
		}
		stamp(func(s *BuildStats) *time.Duration { return &s.RequestMatcherLower }, reqLowerStart)
		reqCompileStart := time.Now()
		s.reqMatcher, err = reqMatcherBuilder.Build()
		if err != nil {
			return nil, fmt.Errorf("failed to build DNS request routing: %w", err)
		}
		stamp(func(s *BuildStats) *time.Duration { return &s.RequestMatcherCompile }, reqCompileStart)
	}

	respProgStart := time.Now()
	responseProgram, err := routing.NewNormalizedProgram(dns.Routing.Response.Rules, dns.Routing.Response.Fallback,
		datReaderOptimizer,
		&routing.MergeAndSortRulesOptimizer{},
		&routing.DeduplicateParamsOptimizer{},
	)
	if err != nil {
		return nil, err
	}
	stamp(func(s *BuildStats) *time.Duration { return &s.ResponseProgramNormalize }, respProgStart)

	// Parse response routing.
	respLowerStart := time.Now()
	respMatcherBuilder, err := NewResponseMatcherBuilderFromProgram(opt.Logger, responseProgram, upstreamName2Id)
	if err != nil {
		return nil, fmt.Errorf("failed to build DNS response routing: %w", err)
	}
	stamp(func(s *BuildStats) *time.Duration { return &s.ResponseMatcherLower }, respLowerStart)
	respCompileStart := time.Now()
	s.respMatcher, err = respMatcherBuilder.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build DNS response routing: %w", err)
	}
	stamp(func(s *BuildStats) *time.Duration { return &s.ResponseMatcherCompile }, respCompileStart)

	if len(dns.Upstream) == 0 {
		// Immediately ready.
		go func() { _ = opt.UpstreamReadyCallback(nil) }()
	}
	return s, nil
}

func datReaderOptimizerForDNS(opt *NewOption) *routing.DatReaderOptimizer {
	if opt != nil && opt.DatReaderOptimizer != nil {
		return opt.DatReaderOptimizer
	}
	return &routing.DatReaderOptimizer{Logger: opt.Logger, LocationFinder: opt.LocationFinder}
}

func (s *Dns) CheckUpstreamsFormat() error {
	for _, upstream := range s.upstream {
		_, hostname, _, _, err := ParseRawUpstream(upstream.Raw)
		if err != nil {
			return err
		}
		if _, err := netip.ParseAddr(hostname); err != nil && upstream.ResolveIp46 == nil {
			return fmt.Errorf("dns upstream %q requires global.bootstrap_resolver because hostname %q is not an IP address", upstream.Raw.String(), hostname)
		}
	}
	return nil
}

func (s *Dns) InitUpstreams(ctx context.Context) {
	var wg sync.WaitGroup
	for _, upstream := range s.upstream {
		wg.Add(1)
		go func(upstream *UpstreamResolver) {
			_, err := upstream.GetUpstream(ctx)
			if err != nil {
				s.log.WithError(err).Debugln("Dns.GetUpstream")
			}
			wg.Done()
		}(upstream)
	}
	wg.Wait()
}

func (s *Dns) RequestSelect(ctx context.Context, qname string, qtype uint16) (upstreamIndex consts.DnsRequestOutboundIndex, upstream *Upstream, err error) {
	// Route.
	upstreamIndex, err = s.reqMatcher.Match(qname, qtype)
	if err != nil {
		return 0, nil, err
	}
	// nil indicates AsIs, Reject, or FakeIP.
	if upstreamIndex == consts.DnsRequestOutboundIndex_AsIs ||
		upstreamIndex == consts.DnsRequestOutboundIndex_Reject ||
		upstreamIndex == consts.DnsRequestOutboundIndex_FakeIP {
		return upstreamIndex, nil, nil
	}
	if int(upstreamIndex) >= len(s.upstream) {
		return 0, nil, fmt.Errorf("bad upstream index: %v not in [0, %v]", upstreamIndex, len(s.upstream)-1)
	}
	// Get corresponding upstream.
	upstream, err = s.upstream[upstreamIndex].GetUpstream(ctx)
	if err != nil {
		return 0, nil, err
	}
	return upstreamIndex, upstream, nil
}

func (s *Dns) ResponseSelect(ctx context.Context, msg *dnsmessage.Msg, fromUpstream *Upstream) (upstreamIndex consts.DnsResponseOutboundIndex, upstream *Upstream, err error) {
	if !msg.Response {
		return 0, nil, fmt.Errorf("DNS response expected but DNS request received")
	}

	// Prepare routing.
	var qname string
	var qtype uint16
	var ips []netip.Addr
	if len(msg.Question) == 0 {
		qname = ""
		qtype = 0
	} else {
		q := msg.Question[0]
		qname = q.Name
		qtype = q.Qtype
		for _, ans := range msg.Answer {
			var (
				ip netip.Addr
				ok bool
			)
			switch body := ans.(type) {
			case *dnsmessage.A:
				ip, ok = netip.AddrFromSlice(body.A)
			case *dnsmessage.AAAA:
				ip, ok = netip.AddrFromSlice(body.AAAA)
			}
			if !ok {
				continue
			}
			ips = append(ips, ip)
		}
	}

	fromValue, ok := s.upstream2Index.Load(fromUpstream)
	if !ok {
		fromValue = int(consts.DnsRequestOutboundIndex_AsIs)
	}
	from := fromValue.(int)
	// Route.
	upstreamIndex, err = s.respMatcher.Match(qname, qtype, ips, consts.DnsRequestOutboundIndex(from))
	if err != nil {
		return 0, nil, err
	}
	// Get corresponding upstream if upstream is neither 'accept' nor 'reject'.
	if !upstreamIndex.IsReserved() {
		if int(upstreamIndex) >= len(s.upstream) {
			return 0, nil, fmt.Errorf("bad upstream index: %v not in [0, %v]", upstreamIndex, len(s.upstream)-1)
		}
		upstream, err = s.upstream[upstreamIndex].GetUpstream(ctx)
		if err != nil {
			return 0, nil, err
		}
	} else {
		// Assign explicitly to let coder know.
		upstream = nil
	}
	return upstreamIndex, upstream, nil
}

// GetUpstreamByName returns the upstream resolver identified by its tag name.
// Returns an error if the name is unknown.
func (s *Dns) GetUpstreamByName(ctx context.Context, name string) (*Upstream, error) {
	idx, ok := s.nameToIndex[name]
	if !ok {
		return nil, fmt.Errorf("upstream %q not found", name)
	}
	if int(idx) >= len(s.upstream) {
		return nil, fmt.Errorf("bad upstream index: %v not in [0, %v]", idx, len(s.upstream)-1)
	}
	return s.upstream[idx].GetUpstream(ctx)
}
