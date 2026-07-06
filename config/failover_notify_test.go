/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config

import (
	"testing"

	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

func TestGroup_FailoverNotifyFieldsDecode(t *testing.T) {
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
    recovery_probe_initial: 15s
    recovery_probe_max: 5m
    recovery_successes: 3
    recovery_stable_time: 30s

    failover_notify: bark
    failover_notify_bark_url: "https://api.day.app/TOKEN/"
    failover_notify_bark_url_env: DAE_FAILOVER_BARK_URL
    failover_notify_switch_title: "Switched"
    failover_notify_switch_body: "{group} switched"
    failover_notify_failback_title: "Recovered"
    failover_notify_failback_body: "{group} recovered"
  }
}
`
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	conf, err := New(sections)
	require.NoError(t, err)
	require.Len(t, conf.Group, 1)
	g := conf.Group[0]
	require.Equal(t, "bark", g.FailoverNotify)
	require.Equal(t, "https://api.day.app/TOKEN/", g.FailoverNotifyBarkURL)
	require.Equal(t, "DAE_FAILOVER_BARK_URL", g.FailoverNotifyBarkURLEnv)
	require.Equal(t, "Switched", g.FailoverNotifySwitchTitle)
	require.Equal(t, "{group} switched", g.FailoverNotifySwitchBody)
	require.Equal(t, "Recovered", g.FailoverNotifyFailbackTitle)
	require.Equal(t, "{group} recovered", g.FailoverNotifyFailbackBody)
}

func TestGroup_MissingFailoverNotifyLeavesEmpty(t *testing.T) {
	raw := `
global {}
routing {
  fallback: direct
}
group {
  proxy {
    policy: random
    filter: name(keyword: hk)
  }
}
`
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	conf, err := New(sections)
	require.NoError(t, err)
	require.Len(t, conf.Group, 1)
	g := conf.Group[0]
	require.Equal(t, "", g.FailoverNotify)
	require.Equal(t, "", g.FailoverNotifyBarkURL)
	require.Equal(t, "", g.FailoverNotifyBarkURLEnv)
	require.Equal(t, "", g.FailoverNotifySwitchTitle)
	require.Equal(t, "", g.FailoverNotifySwitchBody)
	require.Equal(t, "", g.FailoverNotifyFailbackTitle)
	require.Equal(t, "", g.FailoverNotifyFailbackBody)
}

// TestGroup_UnknownFailoverNotifyValueNotRejectedAtParseTime documents that
// parse-time decoding accepts any string value for failover_notify; the
// unknown-value rejection happens at the validate stage (see
// failover_notify_validate_test.go and cmd/validate.go).
func TestGroup_UnknownFailoverNotifyValueNotRejectedAtParseTime(t *testing.T) {
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
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	conf, err := New(sections)
	require.NoError(t, err)
	require.Len(t, conf.Group, 1)
	require.Equal(t, "webhook", conf.Group[0].FailoverNotify)
}
