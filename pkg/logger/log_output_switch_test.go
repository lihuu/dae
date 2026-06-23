/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package logger

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestParseLogOutputStateAcceptsEnabledValues(t *testing.T) {
	for _, input := range []string{
		"enabled=1",
		"enabled=true",
		"enabled=on",
		"enabled=yes",
		" enabled = YES \n",
		"\tenabled\t=\tTrue\t\n",
	} {
		t.Run(input, func(t *testing.T) {
			enabled, err := parseLogOutputState([]byte(input))
			if err != nil {
				t.Fatalf("parseLogOutputState() error = %v", err)
			}
			if !enabled {
				t.Fatalf("parseLogOutputState() = false, want true")
			}
		})
	}
}

func TestParseLogOutputStateAcceptsDisabledValues(t *testing.T) {
	for _, input := range []string{
		"enabled=0",
		"enabled=false",
		"enabled=off",
		"enabled=no",
		" enabled = NO \n",
		"\tenabled\t=\tFalse\t\n",
	} {
		t.Run(input, func(t *testing.T) {
			enabled, err := parseLogOutputState([]byte(input))
			if err != nil {
				t.Fatalf("parseLogOutputState() error = %v", err)
			}
			if enabled {
				t.Fatalf("parseLogOutputState() = true, want false")
			}
		})
	}
}

func TestParseLogOutputStateRejectsInvalidOrMissingEnabled(t *testing.T) {
	for _, input := range []string{
		"",
		"disabled=1",
		"enabled=",
		"enabled=maybe",
		"enabled true",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := parseLogOutputState([]byte(input)); err == nil {
				t.Fatalf("parseLogOutputState() error = nil, want error")
			}
		})
	}
}

func TestLoadLogOutputStateFailOpenForMissingAndInvalidStartupState(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-state")
	if !loadLogOutputStateFailOpen(missing) {
		t.Fatalf("loadLogOutputStateFailOpen(missing) = false, want true")
	}

	invalid := writeLogOutputState(t, "enabled=maybe")
	if !loadLogOutputStateFailOpen(invalid) {
		t.Fatalf("loadLogOutputStateFailOpen(invalid) = false, want true")
	}
}

func TestSwitchableWriterForwardsWhenEnabled(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=1")
	sw := NewLogOutputSwitch(statePath)
	var dst bytes.Buffer

	n, err := sw.Wrap(&dst).Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len("hello") {
		t.Fatalf("Write() n = %d, want %d", n, len("hello"))
	}
	if got := dst.String(); got != "hello" {
		t.Fatalf("dst = %q, want %q", got, "hello")
	}
}

func TestSwitchableWriterDiscardsSuccessfullyWhenDisabled(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=0")
	sw := NewLogOutputSwitch(statePath)
	var dst bytes.Buffer

	n, err := sw.Wrap(&dst).Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len("hello") {
		t.Fatalf("Write() n = %d, want %d", n, len("hello"))
	}
	if got := dst.String(); got != "" {
		t.Fatalf("dst = %q, want empty", got)
	}
}

func TestSwitchableWriterPreservesWrappedWriterResultWhenEnabled(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=1")
	sw := NewLogOutputSwitch(statePath)
	wantErr := errors.New("write failed")
	dst := errWriter{n: 2, err: wantErr}

	n, err := sw.Wrap(dst).Write([]byte("hello"))
	if n != 2 {
		t.Fatalf("Write() n = %d, want 2", n)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Write() error = %v, want %v", err, wantErr)
	}
}

func TestRefreshNowKeepsPreviousValueForInvalidRuntimeState(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=0")
	sw := NewLogOutputSwitch(statePath)
	if sw.Enabled() {
		t.Fatalf("Enabled() = true, want false before invalid refresh")
	}

	if err := os.WriteFile(statePath, []byte("enabled=maybe"), 0o644); err != nil {
		t.Fatalf("write invalid state: %v", err)
	}
	sw.refreshNow()
	if sw.Enabled() {
		t.Fatalf("Enabled() = true after invalid refresh, want previous false")
	}
}

func TestRefreshNowKeepsPreviousValueForRuntimeReadError(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=0")
	sw := NewLogOutputSwitch(statePath)
	if sw.Enabled() {
		t.Fatalf("Enabled() = true, want false before read error")
	}

	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove state file: %v", err)
	}
	if err := os.Mkdir(statePath, 0o755); err != nil {
		t.Fatalf("replace state file with directory: %v", err)
	}

	sw.refreshNow()
	if sw.Enabled() {
		t.Fatalf("Enabled() = true after read error, want previous false")
	}
}

func TestRefreshNowDoesNotReparseUnchangedInvalidRuntimeState(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=0")
	sw := NewLogOutputSwitch(statePath)
	if sw.Enabled() {
		t.Fatalf("Enabled() = true, want false before invalid refresh")
	}

	oldParse := sw.parseState
	parseCalls := 0
	sw.parseState = func(b []byte) (bool, error) {
		parseCalls++
		return parseLogOutputState(b)
	}
	t.Cleanup(func() {
		sw.parseState = oldParse
	})

	if err := os.WriteFile(statePath, []byte("enabled=maybe"), 0o644); err != nil {
		t.Fatalf("write invalid state: %v", err)
	}
	sw.refreshNow()
	if parseCalls != 1 {
		t.Fatalf("parse calls after changed invalid state = %d, want 1", parseCalls)
	}
	if sw.Enabled() {
		t.Fatalf("Enabled() = true after invalid refresh, want previous false")
	}

	sw.refreshNow()
	if parseCalls != 1 {
		t.Fatalf("parse calls after unchanged invalid state = %d, want 1", parseCalls)
	}
	if sw.Enabled() {
		t.Fatalf("Enabled() = true after unchanged invalid refresh, want previous false")
	}
}

func TestRefreshNowFailOpenForMissingRuntimeState(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=0")
	sw := NewLogOutputSwitch(statePath)
	if sw.Enabled() {
		t.Fatalf("Enabled() = true, want false before missing refresh")
	}

	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove state: %v", err)
	}
	sw.refreshNow()
	if !sw.Enabled() {
		t.Fatalf("Enabled() = false after missing refresh, want true")
	}
}

func TestNilLogOutputSwitchEnabledFailOpen(t *testing.T) {
	var sw *LogOutputSwitch
	if !sw.Enabled() {
		t.Fatalf("Enabled() = false, want true for nil switch")
	}
}

func TestSwitchableWriterWrapNilUsesDiscard(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=1")
	sw := NewLogOutputSwitch(statePath)

	n, err := sw.Wrap(nil).Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len("hello") {
		t.Fatalf("Write() n = %d, want %d", n, len("hello"))
	}
}

func TestSetLoggerUsesDefaultSwitchWithoutChangingLevelSemantics(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=0")
	sw := NewLogOutputSwitch(statePath)
	defer useDefaultLogOutputSwitchForTest(sw)()

	var dst bytes.Buffer
	log := logrus.New()
	log.SetOutput(&dst)

	SetLogger(log, "warn", true, nil)
	log.Warn("hidden while disabled")
	if got := dst.String(); got != "" {
		t.Fatalf("log output while switch disabled = %q, want empty", got)
	}

	if err := os.WriteFile(statePath, []byte("enabled=1"), 0o644); err != nil {
		t.Fatalf("enable state: %v", err)
	}
	sw.refreshNow()

	log.Info("still hidden by level")
	log.Warn("visible after enabled")
	got := dst.String()
	if strings.Contains(got, "still hidden by level") {
		t.Fatalf("info output at warn level = %q, want suppressed", got)
	}
	if !strings.Contains(got, "visible after enabled") {
		t.Fatalf("log output after switch enabled = %q, want warn message", got)
	}
}

func TestSetLoggerSharesDefaultSwitchAcrossDedicatedAndStandardLoggers(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=0")
	sw := NewLogOutputSwitch(statePath)
	defer useDefaultLogOutputSwitchForTest(sw)()

	standard := logrus.StandardLogger()
	oldOut := standard.Out
	oldFormatter := standard.Formatter
	oldLevel := standard.Level
	t.Cleanup(func() {
		standard.SetOutput(oldOut)
		standard.SetFormatter(oldFormatter)
		standard.SetLevel(oldLevel)
	})

	var dedicatedDst bytes.Buffer
	dedicated := logrus.New()
	dedicated.SetOutput(&dedicatedDst)

	var standardDst bytes.Buffer
	standard.SetOutput(&standardDst)

	SetLogger(dedicated, "info", true, nil)
	SetLogger(standard, "info", true, nil)

	dedicated.Info("dedicated hidden")
	standard.Info("standard hidden")
	if dedicatedDst.Len() != 0 || standardDst.Len() != 0 {
		t.Fatalf("disabled default switch wrote dedicated=%q standard=%q, want both empty", dedicatedDst.String(), standardDst.String())
	}

	if err := os.WriteFile(statePath, []byte("enabled=1"), 0o644); err != nil {
		t.Fatalf("enable state: %v", err)
	}
	sw.refreshNow()

	dedicated.Info("dedicated visible")
	standard.Info("standard visible")
	if !strings.Contains(dedicatedDst.String(), "dedicated visible") {
		t.Fatalf("dedicated output = %q, want visible message", dedicatedDst.String())
	}
	if !strings.Contains(standardDst.String(), "standard visible") {
		t.Fatalf("standard output = %q, want visible message", standardDst.String())
	}
}

func TestWrapReturnsExistingSwitchableWriterForSameSwitch(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=1")
	sw := NewLogOutputSwitch(statePath)
	var dst bytes.Buffer

	wrapped := sw.Wrap(&dst)
	wrappedAgain := sw.Wrap(wrapped)
	if wrappedAgain != wrapped {
		t.Fatalf("Wrap(existing switchable writer) returned %T, want original %T", wrappedAgain, wrapped)
	}
}

func TestStartWatchingIsSafeToCallMoreThanOnce(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=1")
	sw := NewLogOutputSwitch(statePath)
	defer sw.StopWatching()

	sw.StartWatching()
	sw.StartWatching()
}

func TestStopWatchingIsSafeToCallMoreThanOnce(t *testing.T) {
	statePath := writeLogOutputState(t, "enabled=1")
	sw := NewLogOutputSwitch(statePath)

	sw.StartWatching()
	sw.StopWatching()
	sw.StopWatching()
}

type errWriter struct {
	n   int
	err error
}

func useDefaultLogOutputSwitchForTest(sw *LogOutputSwitch) func() {
	defaultLogOutputSwitchMu.Lock()
	previous := defaultLogOutputSwitch
	defaultLogOutputSwitch = sw
	defaultLogOutputSwitchMu.Unlock()

	return func() {
		if sw != nil {
			sw.StopWatching()
		}

		defaultLogOutputSwitchMu.Lock()
		defaultLogOutputSwitch = previous
		defaultLogOutputSwitchMu.Unlock()
	}
}

func (w errWriter) Write([]byte) (int, error) {
	return w.n, w.err
}

func writeLogOutputState(t *testing.T, state string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "log-output-state")
	if err := os.WriteFile(path, []byte(state), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
	return path
}
