/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config_parser

import (
	"fmt"
	"testing"
)

func TestSyntheticRoutingProfileCases(t *testing.T) {
	for _, shape := range []string{
		"domain-simple",
		"domain-suffix",
		"domain-quoted",
		"dip-simple",
		"and-two",
		"and-three",
		"routing-only-domain-bare",
		"routing-only-domain-kv",
		"routing-only-domain-kv-nofallback",
		"routing-only-domain-kv-with-declarations",
		"routing-only-domain-kv-with-multiparam",
		"global-routing-domain-kv",
		"routing-only-domain-kv-direct",
		"routing-only-domain-kv-block",
	} {
		t.Run(shape, func(t *testing.T) {
			cfg := syntheticRoutingProfileCase(shape, 3)
			_, profile, err := parseWithProfile(cfg)
			if err != nil {
				t.Fatalf("parseWithProfile: %v", err)
			}
			if profile.RoutingRules != 3 {
				t.Fatalf("profile.RoutingRules = %d, want 3", profile.RoutingRules)
			}
			if profile.Functions == 0 {
				t.Fatal("profile.Functions = 0, want non-zero")
			}
		})
	}
}

func TestSyntheticRoutingProfileCase_BuildsRequestedShape(t *testing.T) {
	cfg := syntheticRoutingProfileCase("domain-suffix", 3)
	_, profile, err := parseWithProfile(cfg)
	if err != nil {
		t.Fatalf("parseWithProfile: %v", err)
	}
	if profile.RoutingRules != 3 {
		t.Fatalf("profile.RoutingRules = %d, want 3", profile.RoutingRules)
	}
	if profile.Functions != 6 {
		t.Fatalf("profile.Functions = %d, want 6", profile.Functions)
	}
}

func TestSyntheticRoutingProfileCase_RejectsUnknownShape(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("syntheticRoutingProfileCase did not panic for unknown shape")
		}
	}()
	_ = syntheticRoutingProfileCase("unknown", 1)
}

func TestSyntheticRoutingProfileCase_LiveLikeShapeCounts(t *testing.T) {
	cfg := syntheticRoutingProfileCase("live-like", 2067)
	_, profile, err := parseWithProfile(cfg)
	if err != nil {
		t.Fatalf("parseWithProfile: %v", err)
	}
	if profile.RoutingRules != 2067 {
		t.Fatalf("profile.RoutingRules = %d, want 2067", profile.RoutingRules)
	}
	if profile.Sections != 2 {
		t.Fatalf("profile.Sections = %d, want 2", profile.Sections)
	}
	if profile.Items == 0 {
		t.Fatal("profile.Items = 0, want non-zero")
	}
}

func Example_syntheticRoutingProfileCase() {
	_, profile, err := parseWithProfile(syntheticRoutingProfileCase("domain-simple", 2))
	fmt.Println(err == nil, profile.RoutingRules, profile.Sections)
	// Output: true 2 2
}
