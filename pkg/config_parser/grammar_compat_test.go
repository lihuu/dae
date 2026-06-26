/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config_parser

import (
	"fmt"
	"strings"
	"testing"
)

func TestGrammarInputStarCompatibility(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		sections   []string
		ruleCounts []int
	}{
		{
			name:       "empty input remains valid",
			input:      "\n# comments only\n",
			sections:   nil,
			ruleCounts: nil,
		},
		{
			name: "include section with relative file literal",
			input: `include {
    rules.d/routing.dae
}
`,
			sections:   []string{"include"},
			ruleCounts: []int{0},
		},
		{
			name: "multiple top level sections",
			input: `global {
    tproxy_port: 12345
}

routing {
    domain(example.test) -> proxy
    fallback: direct
}
`,
			sections:   []string{"global", "routing"},
			ruleCounts: []int{0, 1},
		},
		{
			name: "routing only section",
			input: `routing {
    domain(example.test) -> proxy
    domain(suffix:example.org) -> direct
    fallback: proxy
}
`,
			sections:   []string{"routing"},
			ruleCounts: []int{2},
		},
		{
			name: "nested expression remains parseable",
			input: `group {
    proxy {
        filter: name(keyword:hk)
        policy: fixed(0)
    }
}
`,
			sections:   []string{"group"},
			ruleCounts: []int{0},
		},
		{
			name: "mixed subscription node group and routing shape",
			input: `subscription {
    'https://example.invalid/sub'
}

node {
    'socks5://127.0.0.1:1080#local'
}

group {
    proxy {
        filter: name(keyword:local) && !name(keyword:bad)
        policy: fixed(0) [must:true]
    }
}

routing {
    sip(192.168.0.0/24) && !sip(192.168.0.252/30) -> direct
    domain(suffix:example.com) -> proxy
    fallback: direct
}
`,
			sections:   []string{"subscription", "node", "group", "routing"},
			ruleCounts: []int{0, 0, 0, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sections, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(sections) != len(tt.sections) {
				t.Fatalf("len(sections) = %d, want %d", len(sections), len(tt.sections))
			}
			for i, wantName := range tt.sections {
				if sections[i].Name != wantName {
					t.Fatalf("sections[%d].Name = %q, want %q", i, sections[i].Name, wantName)
				}
				if gotRules := countRoutingRuleItems(sections[i]); gotRules != tt.ruleCounts[i] {
					t.Fatalf("section %q routing rules = %d, want %d", wantName, gotRules, tt.ruleCounts[i])
				}
			}
		})
	}
}

func TestGrammarInputStarParsesLargeRoutingOnlyConfig(t *testing.T) {
	const rules = 2067
	sections, profile, err := parseWithProfile(syntheticRoutingProfileCase("routing-only-domain-kv", rules))
	if err != nil {
		t.Fatalf("parseWithProfile() error = %v", err)
	}
	if len(sections) != 1 || sections[0].Name != "routing" {
		t.Fatalf("sections = %#v, want one routing section", sectionNames(sections))
	}
	if profile.RoutingRules != rules {
		t.Fatalf("profile.RoutingRules = %d, want %d", profile.RoutingRules, rules)
	}
	if profile.Sections != 1 {
		t.Fatalf("profile.Sections = %d, want 1", profile.Sections)
	}
}

func TestGrammarInputStarPreservesDigitPrefixDomainHint(t *testing.T) {
	_, err := Parse(`fixed_domain_ttl {
    123.com:60
}
`)
	if err == nil {
		t.Fatal("Parse() succeeded, want digit-prefix domain error")
	}
	msg := err.Error()
	for _, want := range []string{"Hint:", "123.com:60", "'123.com:60'"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q:\n%s", want, msg)
		}
	}
}

func TestGrammarInputStarPreservesRepresentativeSyntaxErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "missing closing brace",
			in: `routing {
    domain(example.test) -> proxy
`,
			want: "missing '}'",
		},
		{
			name: "invalid top level token",
			in:   `123bad`,
			want: "expecting <EOF>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.in)
			if err == nil {
				t.Fatal("Parse() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.want)
			}
		})
	}
}

func countRoutingRuleItems(section *Section) int {
	count := 0
	for _, item := range section.Items {
		if _, ok := item.Value.(*RoutingRule); ok {
			count++
		}
	}
	return count
}

func sectionNames(sections []*Section) []string {
	names := make([]string, 0, len(sections))
	for _, section := range sections {
		names = append(names, fmt.Sprintf("%s(%d)", section.Name, countRoutingRuleItems(section)))
	}
	return names
}
