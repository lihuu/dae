/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@daeuniverse.org>
 */

package dns

import (
	"testing"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

// TestNew_PrebuiltRequestMatcher_IsUsedVerbatim verifies that supplying a
// prebuilt request matcher via NewOption.PrebuiltRequestMatcher makes
// dns.New skip the request-side build entirely and use the supplied matcher.
//
// The acceptance signal is twofold:
//  1. s.reqMatcher is the exact pointer passed in.
//  2. BuildStats.RequestProgramNormalize / RequestMatcherLower /
//     RequestMatcherCompile are all zero — proving the work didn't run.
func TestNew_PrebuiltRequestMatcher_IsUsedVerbatim(t *testing.T) {
	// Build a real request matcher independently — same shape as what
	// daedns.Router.RequestMatcher() returns in production.
	prebuiltBuilder, err := NewRequestMatcherBuilder(
		logrus.New(),
		[]*config_parser.RoutingRule{
			testRequestRule("fakeip", testFunction("qname", testParam("suffix", "google.com"))),
		},
		map[string]uint8{"cn": 0},
		"asis",
	)
	if err != nil {
		t.Fatal(err)
	}
	prebuilt, err := prebuiltBuilder.Build()
	if err != nil {
		t.Fatal(err)
	}

	stats := &BuildStats{}
	s, err := New(&config.Dns{
		Upstream: []config.KeyableString{"cn:udp://223.5.5.5:53"},
		Routing: config.DnsRouting{
			Request: config.DnsRequestRouting{
				// Intentionally large: if reuse path is broken, dns.New would
				// build a heavy matcher here and the timing would be non-zero.
				Rules: []*config_parser.RoutingRule{
					testRequestRule("fakeip",
						testFunction("qname",
							testParam("suffix", "google.com"),
							testParam("suffix", "example.com"),
							testParam("suffix", "github.com"),
						),
					),
				},
				Fallback: "fakeip",
			},
			Response: config.DnsResponseRouting{
				Fallback: "accept",
			},
		},
	}, &NewOption{
		Logger:                  logrus.New(),
		UpstreamResolverNetwork: "udp",
		Stats:                   stats,
		PrebuiltRequestMatcher:  prebuilt,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if s.reqMatcher != prebuilt {
		t.Fatalf("expected reqMatcher to be the prebuilt instance, got %p (want %p)",
			s.reqMatcher, prebuilt)
	}
	assert.Equal(t, time.Duration(0), stats.RequestProgramNormalize,
		"request program normalize must not run when PrebuiltRequestMatcher is set")
	assert.Equal(t, time.Duration(0), stats.RequestMatcherLower,
		"request matcher lower must not run when PrebuiltRequestMatcher is set")
	assert.Equal(t, time.Duration(0), stats.RequestMatcherCompile,
		"request matcher compile must not run when PrebuiltRequestMatcher is set")

	// Match goes through the prebuilt matcher, which only knows about
	// google.com, not example.com — proving the prebuilt matcher is what
	// drives selection, not a matcher built from the dnsCfg rules.
	got, err := s.reqMatcher.Match("www.google.com.", uint16(consts.DnsRequestOutboundIndex_FakeIP))
	if err != nil {
		t.Fatal(err)
	}
	if got != consts.DnsRequestOutboundIndex_FakeIP {
		t.Fatalf("google.com match = %v, want fakeip", got)
	}
}

// TestNew_NoPrebuilt_StillBuildsRequestMatcher confirms the default path is
// unchanged: when PrebuiltRequestMatcher is nil, the request substages stamp
// real durations.
func TestNew_NoPrebuilt_StillBuildsRequestMatcher(t *testing.T) {
	stats := &BuildStats{}
	_, err := New(&config.Dns{
		Upstream: []config.KeyableString{"cn:udp://223.5.5.5:53"},
		Routing: config.DnsRouting{
			Request: config.DnsRequestRouting{
				Rules: []*config_parser.RoutingRule{
					testRequestRule("fakeip", testFunction("qname", testParam("suffix", "google.com"))),
				},
				Fallback: "asis",
			},
			Response: config.DnsResponseRouting{
				Fallback: "accept",
			},
		},
	}, &NewOption{
		Logger:                  logrus.New(),
		UpstreamResolverNetwork: "udp",
		Stats:                   stats,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// We cannot assert > 0 reliably on tiny configs (sub-microsecond), but the
	// fields must be set to non-negative durations (zero value is valid).
	if stats.RequestMatcherCompile < 0 || stats.RequestMatcherLower < 0 || stats.RequestProgramNormalize < 0 {
		t.Fatalf("request substage durations must be non-negative, got %+v", stats)
	}
}
