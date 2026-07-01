/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@daeuniverse.org>
 */

package dns

import (
	"path/filepath"
	"testing"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/routing/domain_matcher"
	"github.com/daeuniverse/dae/pkg/config_parser"
)

func TestRequestMatcherCacheKeyChangesWhenQNameRulesChange(t *testing.T) {
	log := discardLogger()
	cache := domain_matcher.NewTrieCache(log, filepath.Join(t.TempDir(), "trie-cache.bin"), true)
	geositeHash := []byte("0123456789abcdef0123456789abcdef")

	first, err := NewRequestMatcherBuilder(
		log,
		[]*config_parser.RoutingRule{
			testRequestRule("fakeip", testFunction("qname", testParam("suffix", "google.com"))),
		},
		map[string]uint8{},
		"asis",
	)
	if err != nil {
		t.Fatal(err)
	}
	first = first.WithCache(cache, geositeHash)
	firstMatcher, err := first.Build()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := firstMatcher.Match("www.google.com.", 1); err != nil || got != consts.DnsRequestOutboundIndex_FakeIP {
		t.Fatalf("first matcher google.com = (%v, %v), want fakeip", got, err)
	}

	second, err := NewRequestMatcherBuilder(
		log,
		[]*config_parser.RoutingRule{
			testRequestRule("fakeip", testFunction("qname", testParam("suffix", "openai.com"))),
		},
		map[string]uint8{},
		"asis",
	)
	if err != nil {
		t.Fatal(err)
	}
	second = second.WithCache(cache, geositeHash)
	secondMatcher, err := second.Build()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := secondMatcher.Match("api.openai.com.", 1); err != nil || got != consts.DnsRequestOutboundIndex_FakeIP {
		t.Fatalf("second matcher openai.com = (%v, %v), want fakeip after qname rule change", got, err)
	}
}
