/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config_parser

import (
	"os"
	"testing"
)

func TestParseProfileFromFile(t *testing.T) {
	path := os.Getenv(parseBenchConfigEnv)
	if path == "" {
		t.Skipf("set %s=/path/to/config.dae to profile a real config", parseBenchConfigEnv)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sections, profile, err := parseWithProfile(string(content))
	if err != nil {
		t.Fatalf("parseWithProfile %s: %v", path, err)
	}
	if len(sections) == 0 {
		t.Fatalf("parseWithProfile %s returned no sections", path)
	}
	t.Logf("path=%s", path)
	t.Logf("bytes=%d tokens=%d sections=%d items=%d routing_rules=%d functions=%d params=%d",
		profile.Bytes, profile.Tokens, profile.Sections, profile.Items, profile.RoutingRules, profile.Functions, profile.Params)
	t.Logf("input_stream=%s lexer_create=%s token_fill=%s parser_create=%s parser_start=%s walk=%s total_profiled=%s",
		profile.InputStreamDuration,
		profile.LexerCreateDuration,
		profile.TokenFillDuration,
		profile.ParserCreateDuration,
		profile.ParseDuration,
		profile.WalkDuration,
		profile.InputStreamDuration+profile.LexerCreateDuration+profile.TokenFillDuration+profile.ParserCreateDuration+profile.ParseDuration+profile.WalkDuration)
}
