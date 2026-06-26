/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config_parser

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const parseBenchConfigEnv = "DAE_PARSE_BENCH_CONFIG"

func BenchmarkParseFromFile(b *testing.B) {
	path := os.Getenv(parseBenchConfigEnv)
	if path == "" {
		b.Skipf("set %s=/path/to/config.dae to benchmark a real config", parseBenchConfigEnv)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		b.Fatalf("read %s: %v", path, err)
	}
	input := string(content)
	if _, err := Parse(input); err != nil {
		b.Fatalf("parse %s: %v", path, err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sections, err := Parse(input)
		if err != nil {
			b.Fatalf("parse %s: %v", path, err)
		}
		if len(sections) == 0 {
			b.Fatalf("parse %s returned no sections", path)
		}
	}
}

func BenchmarkParseSyntheticRoutingRules(b *testing.B) {
	for _, rules := range []int{100, 500, 1000, 2000, 4000} {
		b.Run(fmt.Sprintf("rules_%d", rules), func(b *testing.B) {
			input := syntheticRoutingConfig(rules)
			if _, err := Parse(input); err != nil {
				b.Fatalf("parse synthetic config: %v", err)
			}

			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sections, err := Parse(input)
				if err != nil {
					b.Fatalf("parse synthetic config: %v", err)
				}
				if len(sections) == 0 {
					b.Fatal("parse synthetic config returned no sections")
				}
			}
		})
	}
}

func BenchmarkParseSyntheticRoutingShapes(b *testing.B) {
	shapes := []string{
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
	}
	for _, shape := range shapes {
		shape := shape
		b.Run(shape, func(b *testing.B) {
			input := syntheticRoutingProfileCase(shape, 2000)
			if _, err := Parse(input); err != nil {
				b.Fatalf("parse synthetic %s config: %v", shape, err)
			}

			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sections, err := Parse(input)
				if err != nil {
					b.Fatalf("parse synthetic %s config: %v", shape, err)
				}
				if len(sections) == 0 {
					b.Fatalf("parse synthetic %s config returned no sections", shape)
				}
			}
		})
	}
}

func syntheticRoutingConfig(rules int) string {
	var b strings.Builder
	b.Grow(64 + rules*64)
	b.WriteString("global {\n}\n\nrouting {\n")
	for i := range rules {
		fmt.Fprintf(&b, "    domain(bench-%04d.example.com) -> direct\n", i)
	}
	b.WriteString("    fallback: direct\n}\n")
	return b.String()
}

func syntheticRoutingProfileCase(shape string, rules int) string {
	var b strings.Builder
	b.Grow(64 + rules*96)
	if shape == "routing-only-domain-bare" ||
		shape == "routing-only-domain-kv" ||
		shape == "routing-only-domain-kv-nofallback" ||
		shape == "routing-only-domain-kv-with-declarations" ||
		shape == "routing-only-domain-kv-with-multiparam" ||
		shape == "routing-only-domain-kv-direct" ||
		shape == "routing-only-domain-kv-block" {
		b.WriteString("routing {\n")
	} else {
		b.WriteString("global {\n}\n\nrouting {\n")
	}
	for i := range rules {
		switch shape {
		case "domain-simple", "routing-only-domain-bare":
			fmt.Fprintf(&b, "    domain(bench-%04d.example.com) -> direct\n", i)
		case "domain-suffix", "routing-only-domain-kv":
			fmt.Fprintf(&b, "    domain(suffix:bench-%04d.example.com) -> proxy\n", i)
		case "routing-only-domain-kv-nofallback":
			fmt.Fprintf(&b, "    domain(suffix:bench-%04d.example.com) -> proxy\n", i)
		case "routing-only-domain-kv-with-declarations":
			if i%100 == 0 {
				fmt.Fprintf(&b, "    note_%04d: value_%04d\n", i, i)
			}
			fmt.Fprintf(&b, "    domain(suffix:bench-%04d.example.com) -> proxy\n", i)
		case "routing-only-domain-kv-with-multiparam":
			if i%100 == 0 {
				fmt.Fprintf(&b, "    domain(suffix:bench-%04d.example.com, suffix:alt-%04d.example.com, full:exact-%04d.example.com) -> direct\n", i, i, i)
			} else {
				fmt.Fprintf(&b, "    domain(suffix:bench-%04d.example.com) -> proxy\n", i)
			}
		case "global-routing-domain-kv":
			fmt.Fprintf(&b, "    domain(suffix:bench-%04d.example.com) -> proxy\n", i)
		case "routing-only-domain-kv-direct":
			fmt.Fprintf(&b, "    domain(suffix:bench-%04d.example.com) -> direct\n", i)
		case "routing-only-domain-kv-block":
			fmt.Fprintf(&b, "    domain(suffix:bench-%04d.example.com) -> block\n", i)
		case "domain-quoted":
			fmt.Fprintf(&b, "    domain('bench-%04d.example.com') -> proxy\n", i)
		case "dip-simple":
			fmt.Fprintf(&b, "    dip(198.51.%d.%d) -> direct\n", i/250, i%250+1)
		case "and-two":
			fmt.Fprintf(&b, "    l4proto(tcp) && dport(%d) -> direct\n", 10000+i)
		case "and-three":
			fmt.Fprintf(&b, "    sip(192.0.2.%d) && l4proto(tcp) && dport(%d) -> direct\n", i%250+1, 10000+i)
		case "live-like":
			if i%6 == 0 {
				fmt.Fprintf(&b, "    domain(full:bench-%04d.example.com) -> proxy\n", i)
			} else {
				fmt.Fprintf(&b, "    domain(suffix:bench-%04d.example.com) -> proxy\n", i)
			}
		default:
			panic("unknown synthetic routing shape: " + shape)
		}
	}
	if shape != "routing-only-domain-kv-nofallback" {
		b.WriteString("    fallback: direct\n")
	}
	b.WriteString("}\n")
	return b.String()
}
