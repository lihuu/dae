/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config

import (
	"strings"
	"testing"

	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

// validateFailoverNotifyValues mirrors the validate-stage check implemented in
// cmd/validate.go (validateFailoverNotify): it rejects unknown failover_notify
// values on failover policy groups. The mirror lives in package config so the
// test can reuse the real parser without exporting cmd internals; it cannot
// import component/outbound (import cycle), so it detects a failover group by
// parsing g.Policy with ParseFunctionListOrString and comparing the policy
// function name to consts.DialerSelectionPolicy_Failover ("failover"). The
// cmd-side implementation uses outbound.NewDialerSelectionPolicyFromGroupParam,
// which performs the same parse plus full policy validation; the two must agree
// on the failover-group detection for any well-formed config.
func validateFailoverNotifyValues(t *testing.T, raw string) error {
	t.Helper()
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	conf, err := New(sections)
	require.NoError(t, err)
	for _, g := range conf.Group {
		if g.FailoverNotify == "" || g.FailoverNotify == "bark" {
			continue
		}
		// Only enforce on failover policy groups. Parse g.Policy directly
		// rather than indexing it (FunctionListOrString is `any` and not
		// indexable); "failover" matches consts.DialerSelectionPolicy_Failover.
		fs, err := ParseFunctionListOrString(g.Policy)
		if err != nil || len(fs) != 1 {
			continue
		}
		if fs[0].Name == "failover" {
			return &unknownNotifyError{group: g.Name, value: g.FailoverNotify}
		}
	}
	return nil
}

type unknownNotifyError struct{ group, value string }

func (e *unknownNotifyError) Error() string {
	return "group " + e.group + ": unknown failover_notify " + e.value
}

// TestValidateFailoverNotify_RejectsUnknownOnFailoverGroup is the RED/GREEN
// anchor for the validate-stage check: a failover group with an unknown
// failover_notify value must be rejected.
func TestValidateFailoverNotify_RejectsUnknownOnFailoverGroup(t *testing.T) {
	raw := `
global {}
routing {
  fallback: direct
}
group {
  proxy_failover {
    filter: name(node1) [priority: 0]
    filter: name(node2) [priority: 1]
    policy: failover
    failover_notify: webhook
  }
}
`
	err := validateFailoverNotifyValues(t, raw)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown failover_notify")
	require.True(t, strings.Contains(err.Error(), "webhook"))
}

// TestValidateFailoverNotify_AcceptsBarkOnFailoverGroup confirms "bark" is
// accepted on a failover group (and does not require a Bark URL to parse).
func TestValidateFailoverNotify_AcceptsBarkOnFailoverGroup(t *testing.T) {
	raw := `
global {}
routing {
  fallback: direct
}
group {
  proxy_failover {
    filter: name(node1) [priority: 0]
    filter: name(node2) [priority: 1]
    policy: failover
    failover_notify: bark
    failover_notify_bark_url: "https://api.day.app/TOKEN/"
  }
}
`
	require.NoError(t, validateFailoverNotifyValues(t, raw))
}

// TestValidateFailoverNotify_AcceptsEmptyOnFailoverGroup confirms the default
// (no failover_notify) leaves failover groups untouched.
func TestValidateFailoverNotify_AcceptsEmptyOnFailoverGroup(t *testing.T) {
	raw := `
global {}
routing {
  fallback: direct
}
group {
  proxy_failover {
    filter: name(node1) [priority: 0]
    filter: name(node2) [priority: 1]
    policy: failover
  }
}
`
	require.NoError(t, validateFailoverNotifyValues(t, raw))
}

// TestValidateFailoverNotify_IgnoresUnknownOnNonFailoverGroup confirms the
// check only enforces on failover policy groups: a non-failover group (here,
// random) with an unknown failover_notify value is not rejected at this stage.
func TestValidateFailoverNotify_IgnoresUnknownOnNonFailoverGroup(t *testing.T) {
	raw := `
global {}
routing {
  fallback: direct
}
group {
  proxy {
    policy: random
    filter: name(keyword: hk)
    failover_notify: webhook
  }
}
`
	require.NoError(t, validateFailoverNotifyValues(t, raw))
}
