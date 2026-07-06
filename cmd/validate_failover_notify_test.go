/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"strings"
	"testing"

	"github.com/daeuniverse/dae/config"
	"github.com/stretchr/testify/require"
)

// failoverGroup builds an in-memory Group whose policy parses to failover, so
// validateFailoverNotify exercises its failover-group enforcement path without
// needing the config parser. Setting Policy to the string "failover" is the
// same shape config_parser produces for `policy: failover`.
func failoverGroup(name string, notify string) config.Group {
	return config.Group{
		Name:           name,
		Policy:         "failover",
		FailoverNotify: notify,
	}
}

// TestValidateFailoverNotify_AcceptsBarkAndEmpty confirms "" and "bark" are
// accepted on failover groups.
func TestValidateFailoverNotify_AcceptsBarkAndEmpty(t *testing.T) {
	conf := &config.Config{}
	conf.Group = []config.Group{
		failoverGroup("g1", ""),
		failoverGroup("g2", "bark"),
	}
	require.NoError(t, validateFailoverNotify(conf))
}

// TestValidateFailoverNotify_RejectsUnknownOnFailoverGroup is the RED/GREEN
// anchor for the cmd-side check: a failover group with an unknown
// failover_notify value must be rejected, and the error must name both the
// group and the offending value.
func TestValidateFailoverNotify_RejectsUnknownOnFailoverGroup(t *testing.T) {
	conf := &config.Config{}
	conf.Group = []config.Group{
		failoverGroup("proxy_failover", "webhook"),
	}
	err := validateFailoverNotify(conf)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown failover_notify")
	require.True(t, strings.Contains(err.Error(), "webhook"))
	require.True(t, strings.Contains(err.Error(), "proxy_failover"))
}

// TestValidateFailoverNotify_IgnoresUnknownOnNonFailoverGroup confirms the
// check only enforces on failover policy groups: a random-policy group with an
// unknown failover_notify value is not rejected.
func TestValidateFailoverNotify_IgnoresUnknownOnNonFailoverGroup(t *testing.T) {
	conf := &config.Config{}
	conf.Group = []config.Group{
		{Name: "proxy", Policy: "random", FailoverNotify: "webhook"},
	}
	require.NoError(t, validateFailoverNotify(conf))
}

// TestValidateFailoverNotify_NoGroups confirms the check is a no-op when there
// are no groups (the default minimal config).
func TestValidateFailoverNotify_NoGroups(t *testing.T) {
	conf := &config.Config{}
	require.NoError(t, validateFailoverNotify(conf))
}
