/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package domain_matcher

import (
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"golang.org/x/exp/slices"
)

func TestAhocorasickSlimtrie(t *testing.T) {

	logrus.SetLevel(logrus.TraceLevel)
	simulatedDomainSet, err := getDomain()
	if err != nil {
		if strings.Contains(err.Error(), "geosite.dat: file does not exist") {
			t.Skipf("skip due to missing geosite.dat in test environment: %v", err)
		}
		t.Fatal(err)
	}
	bf := NewBruteforce(consts.MaxMatchSetLen)
	actrie := NewAhocorasickSlimtrie(logrus.StandardLogger(), consts.MaxMatchSetLen)
	for _, domains := range simulatedDomainSet {
		bf.AddSet(domains.RuleIndex, domains.Domains, domains.Key)
		actrie.AddSet(domains.RuleIndex, domains.Domains, domains.Key)
	}
	if err = bf.Build(); err != nil {
		t.Fatal(err)
	}
	if err = actrie.Build(); err != nil {
		t.Fatal(err)
	}

	r := rand.New(rand.NewSource(200))
	for i := range 10000 {
		sample := TestSample[r.Intn(len(TestSample))]
		choice := r.Intn(10)
		switch {
		case choice < 4:
			addN := r.Intn(5)
			buf := make([]byte, addN)
			for i := range buf {
				buf[i] = 'a' + byte(r.Intn('z'-'a'))
			}
			sample = string(buf) + "." + sample
		case choice >= 4 && choice < 6:
			k := r.Intn(len(sample))
			sample = sample[k:]
		default:
		}
		bitmap := bf.MatchDomainBitmap(sample)
		bitmap2 := actrie.MatchDomainBitmap(sample)
		if !slices.Equal(bitmap, bitmap2) {
			t.Fatal(i, sample, bitmap, bitmap2)
		}
	}
}

// TestAhocorasickSlimtrie_BuildStats_PopulatesPerSlotShape verifies that
// WithStats causes Build to populate slot counts, pattern counts, max-slot
// size, and the time-like fields with sane invariants. The test deliberately
// supplies one large suffix slot and one keyword slot so AC and trie both
// have non-zero work; assertions stay on invariants (>= relationships) rather
// than absolute times, which would be flaky.
func TestAhocorasickSlimtrie_BuildStats_PopulatesPerSlotShape(t *testing.T) {
	stats := &BuildStats{}
	m := NewAhocorasickSlimtrie(logrus.New(), consts.MaxMatchSetLen).WithStats(stats)
	m.AddSet(0, []string{"google.com", "wikipedia.org", "github.com", "youtube.com"}, consts.RoutingDomainKey_Suffix)
	m.AddSet(1, []string{"adservice", "tracker", "analytics"}, consts.RoutingDomainKey_Keyword)
	m.AddSet(2, []string{"www.iana.org"}, consts.RoutingDomainKey_Full)
	if err := m.Build(); err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	// Suffix patterns expand to ".d$" + "^d$" so slot 0 contributes 8 strings;
	// slot 2 contributes 1 "^d$". Both land in the trie side.
	assert.Equal(t, 2, stats.TrieSlots, "two trie slots (suffix + full)")
	assert.Equal(t, 9, stats.TriePatterns, "suffix expands 2x; one full adds 1")
	assert.Equal(t, 8, stats.TrieMaxSlotPatterns, "biggest trie slot is suffix slot 0")
	assert.Equal(t, 1, stats.AcSlots, "one AC slot (keyword)")
	assert.Equal(t, 3, stats.AcPatterns, "three keywords in slot 1")
	assert.Equal(t, 3, stats.AcMaxSlotPatterns)
	assert.Equal(t, 0, stats.RegexpSlots, "no regex slots")

	// Time invariants: max <= cpu (sum), wall >= max for each side individually.
	assert.GreaterOrEqual(t, stats.TrieCpuDuration, stats.TrieMaxSlotDuration, "cpu >= max slot")
	assert.GreaterOrEqual(t, stats.AcCpuDuration, stats.AcMaxSlotDuration, "cpu >= max slot")
	assert.GreaterOrEqual(t, stats.WallDuration, stats.TrieMaxSlotDuration, "wall >= trie max")
	assert.GreaterOrEqual(t, stats.WallDuration, stats.AcMaxSlotDuration, "wall >= ac max")
}

// TestAhocorasickSlimtrie_BuildStats_NilStatsIsSafe ensures the default path
// (no WithStats call) still compiles a usable matcher and adds zero overhead.
func TestAhocorasickSlimtrie_BuildStats_NilStatsIsSafe(t *testing.T) {
	m := NewAhocorasickSlimtrie(logrus.New(), consts.MaxMatchSetLen)
	m.AddSet(0, []string{"google.com"}, consts.RoutingDomainKey_Suffix)
	if err := m.Build(); err != nil {
		t.Fatalf("Build() error = %v", err)
	}
}

func TestAhocorasickSlimtrie_BuildWithCacheHitPreservesKeywordAndRegex(t *testing.T) {
	cache := NewTrieCache(logrus.New(), filepath.Join(t.TempDir(), "trie-cache.bin"), true)
	sourceHash := []byte("0123456789abcdef0123456789abcdef")

	build := func() *AhocorasickSlimtrie {
		m := NewAhocorasickSlimtrie(logrus.New(), consts.MaxMatchSetLen)
		m.AddSet(0, []string{"example.com"}, consts.RoutingDomainKey_Suffix)
		m.AddSet(1, []string{"tracker"}, consts.RoutingDomainKey_Keyword)
		m.AddSet(2, []string{`^api[0-9]+\.example\.org$`}, consts.RoutingDomainKey_Regex)
		return m
	}

	first := build()
	if err := first.BuildWithCache(cache, sourceHash); err != nil {
		t.Fatalf("first BuildWithCache() error = %v", err)
	}
	if tries, err := cache.Load(sourceHash); err != nil || tries == nil {
		t.Fatalf("cache.Load() after initial build = (%v, %v), want cache hit", tries, err)
	}
	second := build()
	if err := second.BuildWithCache(cache, sourceHash); err != nil {
		t.Fatalf("second BuildWithCache() error = %v", err)
	}

	for _, domain := range []string{
		"www.example.com",
		"metrics.tracker.net",
		"api42.example.org",
	} {
		if got, want := second.MatchDomainBitmap(domain), first.MatchDomainBitmap(domain); !slices.Equal(got, want) {
			t.Fatalf("cache-hit matcher mismatch for %q: got %v, want %v", domain, got, want)
		}
	}
}
