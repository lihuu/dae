/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// testNotifyConfig is a tiny identity helper that keeps the disabled-when-empty
// test case self-documenting: it makes explicit that an empty failover_notify
// value disables notifications.
func testNotifyConfig(v string) string { return v }

// newTestLogger returns a discard-only logger for wiring tests. The dispatcher
// and notifier accept a *logrus.Logger; tests must not depend on a package-level
// log variable (there is none in package control).
func newTestLogger() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.PanicLevel)
	return l
}

// TestBuildFailoverEventCallback_DisabledWhenNoNotify asserts that a group
// without failover_notify gets a nil callback (notifications disabled).
func TestBuildFailoverEventCallback_DisabledWhenNoNotify(t *testing.T) {
	cb, closer := buildFailoverEventCallback(newTestLogger(), testNotifyConfig(""), "", "", "", "", "", "")
	require.Nil(t, cb)
	require.Nil(t, closer)
}

// TestBuildFailoverEventCallback_DisabledWhenBarkButNoURL asserts that
// failover_notify: bark with no URL disables the notifier without error.
func TestBuildFailoverEventCallback_DisabledWhenBarkButNoURL(t *testing.T) {
	cb, closer := buildFailoverEventCallback(newTestLogger(), "bark", "", "", "", "", "", "")
	require.Nil(t, cb)
	require.Nil(t, closer)
}

// TestBuildFailoverEventCallback_EnabledWhenBarkAndURL asserts a non-nil
// callback and closer are returned when bark + URL are configured.
func TestBuildFailoverEventCallback_EnabledWhenBarkAndURL(t *testing.T) {
	cb, closer := buildFailoverEventCallback(
		newTestLogger(),
		"bark",
		"https://api.day.example/token/",
		"", "", "", "", "",
	)
	require.NotNil(t, cb)
	require.NotNil(t, closer)
	defer closer()
}

// TestBuildFailoverEventCallback_EnvURLUsedWhenDirectEmpty asserts env URL
// resolves when direct is empty (URL priority: direct > env).
func TestBuildFailoverEventCallback_EnvURLUsedWhenDirectEmpty(t *testing.T) {
	cb, closer := buildFailoverEventCallback(
		newTestLogger(),
		"bark",
		"",
		"https://api.day.example/env-token/",
		"", "", "", "",
	)
	require.NotNil(t, cb)
	require.NotNil(t, closer)
	defer closer()
}
