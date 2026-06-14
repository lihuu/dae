/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"testing"

	"github.com/daeuniverse/dae/config"
	"github.com/stretchr/testify/require"
)

func makeDnsWithFakeIP(enabled bool, inet4Range, storePath, directUpstream string, ttl int) config.Dns {
	return config.Dns{
		FakeIP: config.DnsFakeIP{
			Enabled:        enabled,
			Inet4Range:     inet4Range,
			TTL:            ttl,
			Store:          storePath,
			DirectUpstream: directUpstream,
		},
	}
}

// TestFakeIPPrefixChangeRejectsReload verifies that changing inet4_range
// (which changes store identity) causes validateFakeIPReloadCompatibility
// to return an error.
func TestFakeIPPrefixChangeRejectsReload(t *testing.T) {
	oldDns := makeDnsWithFakeIP(true, "198.18.0.0/15", "/var/lib/dae/fakeip.db", "cn", 60)
	newDns := makeDnsWithFakeIP(true, "198.51.100.0/24", "/var/lib/dae/fakeip.db", "cn", 60)
	oldConf := &config.Config{Dns: oldDns}
	newConf := &config.Config{Dns: newDns}

	err := validateFakeIPReloadCompatibility(oldConf, newConf)
	require.Error(t, err, "changing inet4_range must require a full restart")
	require.Contains(t, err.Error(), "restart")
}

// TestFakeIPStorePathChangeRejectsReload verifies that changing the store
// path causes validateFakeIPReloadCompatibility to return an error.
func TestFakeIPStorePathChangeRejectsReload(t *testing.T) {
	oldDns := makeDnsWithFakeIP(true, "198.18.0.0/15", "/var/lib/dae/fakeip.db", "cn", 60)
	newDns := makeDnsWithFakeIP(true, "198.18.0.0/15", "/tmp/other-fakeip.db", "cn", 60)
	oldConf := &config.Config{Dns: oldDns}
	newConf := &config.Config{Dns: newDns}

	err := validateFakeIPReloadCompatibility(oldConf, newConf)
	require.Error(t, err, "changing store path must require a full restart")
	require.Contains(t, err.Error(), "restart")
}

// TestFakeIPEnableToggleRejectsReload verifies that enabling or disabling
// FakeIP (which changes store identity) requires a full restart.
func TestFakeIPEnableToggleRejectsReload(t *testing.T) {
	oldDns := makeDnsWithFakeIP(false, "198.18.0.0/15", "/var/lib/dae/fakeip.db", "", 60)
	newDns := makeDnsWithFakeIP(true, "198.18.0.0/15", "/var/lib/dae/fakeip.db", "cn", 60)
	oldConf := &config.Config{Dns: oldDns}
	newConf := &config.Config{Dns: newDns}

	err := validateFakeIPReloadCompatibility(oldConf, newConf)
	require.Error(t, err, "enabling FakeIP must require a full restart")

	// Reverse: disable.
	err = validateFakeIPReloadCompatibility(newConf, oldConf)
	require.Error(t, err, "disabling FakeIP must require a full restart")
}

// TestFakeIPTTLChangeAllowsReload verifies that changing only TTL (which
// does NOT change store identity) passes the compatibility check.
func TestFakeIPTTLChangeAllowsReload(t *testing.T) {
	oldDns := makeDnsWithFakeIP(true, "198.18.0.0/15", "/var/lib/dae/fakeip.db", "cn", 60)
	newDns := makeDnsWithFakeIP(true, "198.18.0.0/15", "/var/lib/dae/fakeip.db", "cn", 300)
	oldConf := &config.Config{Dns: oldDns}
	newConf := &config.Config{Dns: newDns}

	err := validateFakeIPReloadCompatibility(oldConf, newConf)
	require.NoError(t, err, "TTL-only change should be reload-compatible")
}

// TestFakeIPDirectUpstreamChangeAllowsReload verifies that changing only
// direct_upstream (which does NOT change store identity) passes the check.
func TestFakeIPDirectUpstreamChangeAllowsReload(t *testing.T) {
	oldDns := makeDnsWithFakeIP(true, "198.18.0.0/15", "/var/lib/dae/fakeip.db", "cn", 60)
	newDns := makeDnsWithFakeIP(true, "198.18.0.0/15", "/var/lib/dae/fakeip.db", "telecom", 60)
	oldConf := &config.Config{Dns: oldDns}
	newConf := &config.Config{Dns: newDns}

	err := validateFakeIPReloadCompatibility(oldConf, newConf)
	require.NoError(t, err, "direct_upstream-only change should be reload-compatible")
}

// TestFakeIPReloadCompatibilityNilConfigs verifies that nil configs pass
// the compatibility check (no existing or no new config).
func TestFakeIPReloadCompatibilityNilConfigs(t *testing.T) {
	dns := makeDnsWithFakeIP(true, "198.18.0.0/15", "/var/lib/dae/fakeip.db", "cn", 60)
	conf := &config.Config{Dns: dns}

	require.NoError(t, validateFakeIPReloadCompatibility(nil, conf))
	require.NoError(t, validateFakeIPReloadCompatibility(conf, nil))
	require.NoError(t, validateFakeIPReloadCompatibility(nil, nil))
}

// TestFakeIPReloadCompatibilityBothDisabled verifies that when both old and
// new configs have FakeIP disabled, no error is returned.
func TestFakeIPReloadCompatibilityBothDisabled(t *testing.T) {
	oldDns := makeDnsWithFakeIP(false, "", "", "", 0)
	newDns := makeDnsWithFakeIP(false, "", "", "", 0)
	oldConf := &config.Config{Dns: oldDns}
	newConf := &config.Config{Dns: newDns}

	require.NoError(t, validateFakeIPReloadCompatibility(oldConf, newConf))
}
