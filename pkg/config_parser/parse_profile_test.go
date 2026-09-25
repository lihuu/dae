/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config_parser

import "testing"

func TestParseWithProfile_BasicConfigReportsPhases(t *testing.T) {
	sections, profile, err := parseWithProfile("global{log_level: info} routing{fallback: direct}")
	if err != nil {
		t.Fatalf("parseWithProfile: %v", err)
	}
	if len(sections) != 2 {
		t.Fatalf("sections = %d, want 2", len(sections))
	}
	if profile.Bytes == 0 {
		t.Fatal("profile.Bytes = 0, want non-zero")
	}
	if profile.Tokens == 0 {
		t.Fatal("profile.Tokens = 0, want non-zero")
	}
	if profile.Sections != 2 {
		t.Fatalf("profile.Sections = %d, want 2", profile.Sections)
	}
	if profile.Items == 0 {
		t.Fatal("profile.Items = 0, want non-zero")
	}
	if profile.RoutingRules != 0 {
		t.Fatalf("profile.RoutingRules = %d, want 0", profile.RoutingRules)
	}
	if profile.InputStreamDuration == 0 {
		t.Fatal("profile.InputStreamDuration = 0, want non-zero")
	}
	if profile.TokenFillDuration == 0 {
		t.Fatal("profile.TokenFillDuration = 0, want non-zero")
	}
	if profile.ParseDuration == 0 {
		t.Fatal("profile.ParseDuration = 0, want non-zero")
	}
	if profile.WalkDuration == 0 {
		t.Fatal("profile.WalkDuration = 0, want non-zero")
	}
}
