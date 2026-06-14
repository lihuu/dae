/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@daeuniverse.org>
 */

package dns

import (
	"context"
	"testing"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/sirupsen/logrus"
)

func TestRequestMatcherSelectsFakeIP(t *testing.T) {
	builder, err := NewRequestMatcherBuilder(
		logrus.New(),
		nil,
		map[string]uint8{"cn": 0},
		"fakeip",
	)
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	got, err := matcher.Match("www.google.com.", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != consts.DnsRequestOutboundIndex_FakeIP {
		t.Fatalf("got %v, want fakeip", got)
	}
}

func TestRequestMatcherSelectsFakeIPByRule(t *testing.T) {
	rules := []*config_parser.RoutingRule{
		testRequestRule(
			"fakeip",
			testFunction("qname", testParam("suffix", "google.com")),
		),
	}
	builder, err := NewRequestMatcherBuilder(
		logrus.New(),
		rules,
		map[string]uint8{"cn": 0},
		"asis",
	)
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	got, err := matcher.Match("www.google.com.", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != consts.DnsRequestOutboundIndex_FakeIP {
		t.Fatalf("got %v, want fakeip", got)
	}
}

func TestDNSRequestSelectReturnsNilUpstreamForFakeIP(t *testing.T) {
	dnsConf := &config.Dns{
		Upstream: []config.KeyableString{"cn:udp://223.5.5.5:53"},
		Routing: config.DnsRouting{
			Request: config.DnsRequestRouting{
				Fallback: "fakeip",
			},
			Response: config.DnsResponseRouting{
				Fallback: "accept",
			},
		},
	}
	s, err := New(dnsConf, &NewOption{
		Logger:                  logrus.New(),
		UpstreamResolverNetwork: "udp",
	})
	if err != nil {
		t.Fatal(err)
	}
	index, upstream, err := s.RequestSelect(context.Background(), "www.google.com.", 1)
	if err != nil {
		t.Fatal(err)
	}
	if index != consts.DnsRequestOutboundIndex_FakeIP || upstream != nil {
		t.Fatalf("selection = (%v, %v), want (fakeip, nil)", index, upstream)
	}
}

func TestFakeIPOutboundString(t *testing.T) {
	got := consts.DnsRequestOutboundIndex_FakeIP.String()
	if got != "fakeip" {
		t.Fatalf("got %q, want %q", got, "fakeip")
	}
}

func TestFakeIPReducesUserDefinedMax(t *testing.T) {
	if consts.DnsRequestOutboundIndex_UserDefinedMax != 0xFA {
		t.Fatalf("UserDefinedMax = 0x%X, want 0xFA", consts.DnsRequestOutboundIndex_UserDefinedMax)
	}
}
