// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>

package config_parser

import (
	"testing"
)

func TestRoutingParsesRejectOutbound(t *testing.T) {
	config := `
routing {
    domain(suffix: bad.example) -> reject
    fallback: proxy
}
`
	sects, err := Parse(config)
	if err != nil {
		t.Fatalf("Parse err = %v", err)
	}
	var routing *Section
	for _, s := range sects {
		if s.Name == "routing" {
			routing = s
			break
		}
	}
	if routing == nil {
		t.Fatal("no routing section parsed")
	}
	var sawReject bool
	for _, item := range routing.Items {
		if rule, ok := item.Value.(*RoutingRule); ok {
			if rule.Outbound.Name == "reject" {
				sawReject = true
				break
			}
		}
	}
	if !sawReject {
		t.Fatalf("no rule with outbound \"reject\" parsed; items=%+v", routing.Items)
	}
}

func TestRoutingParsesBlockStillWorks(t *testing.T) {
	config := `
routing {
    domain(suffix: bad.example) -> block
    fallback: proxy
}
`
	sects, err := Parse(config)
	if err != nil {
		t.Fatalf("Parse err = %v", err)
	}
	for _, s := range sects {
		if s.Name != "routing" {
			continue
		}
		for _, item := range s.Items {
			if rule, ok := item.Value.(*RoutingRule); ok {
				if rule.Outbound.Name == "block" {
					return // pass
				}
			}
		}
	}
	t.Fatal("no rule with outbound \"block\" parsed")
}
