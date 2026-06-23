/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package logger

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultLogOutputStatePath = "/var/lib/dae/log-output-state"

type LogOutputSwitch struct {
	path string

	enabled atomic.Bool

	watchOnce sync.Once

	mu       sync.Mutex
	lastSize int64
	lastMod  time.Time
}

type SwitchableWriter struct {
	outputSwitch *LogOutputSwitch
	dst          io.Writer
}

func NewLogOutputSwitch(path string) *LogOutputSwitch {
	if path == "" {
		path = DefaultLogOutputStatePath
	}

	sw := &LogOutputSwitch{path: path}
	sw.enabled.Store(loadLogOutputStateFailOpen(path))
	return sw
}

func (sw *LogOutputSwitch) Enabled() bool {
	if sw == nil {
		return true
	}
	return sw.enabled.Load()
}

func (sw *LogOutputSwitch) Wrap(dst io.Writer) io.Writer {
	if dst == nil {
		dst = io.Discard
	}
	return &SwitchableWriter{
		outputSwitch: sw,
		dst:          dst,
	}
}

func (sw *LogOutputSwitch) StartWatching() {
	if sw == nil {
		return
	}
	sw.watchOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for range ticker.C {
				sw.refreshNow()
			}
		}()
	})
}

func (sw *LogOutputSwitch) refreshNow() {
	if sw == nil {
		return
	}

	info, err := os.Stat(sw.path)
	if err != nil {
		if os.IsNotExist(err) {
			sw.enabled.Store(true)
			sw.mu.Lock()
			sw.lastSize = 0
			sw.lastMod = time.Time{}
			sw.mu.Unlock()
		}
		return
	}

	sw.mu.Lock()
	if info.Size() == sw.lastSize && info.ModTime().Equal(sw.lastMod) {
		sw.mu.Unlock()
		return
	}
	sw.mu.Unlock()

	b, err := os.ReadFile(sw.path)
	if err != nil {
		return
	}
	enabled, err := parseLogOutputState(b)
	if err != nil {
		return
	}

	sw.enabled.Store(enabled)
	sw.mu.Lock()
	sw.lastSize = info.Size()
	sw.lastMod = info.ModTime()
	sw.mu.Unlock()
}

func (w *SwitchableWriter) Write(p []byte) (int, error) {
	if w == nil || w.outputSwitch == nil || w.outputSwitch.Enabled() {
		if w == nil || w.dst == nil {
			return io.Discard.Write(p)
		}
		return w.dst.Write(p)
	}
	return len(p), nil
}

func parseLogOutputState(b []byte) (bool, error) {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return false, fmt.Errorf("invalid log output state line %q", line)
		}
		if strings.ToLower(strings.TrimSpace(key)) != "enabled" {
			continue
		}

		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "on", "yes":
			return true, nil
		case "0", "false", "off", "no":
			return false, nil
		case "":
			return false, errors.New("missing enabled value")
		default:
			return false, fmt.Errorf("invalid enabled value %q", strings.TrimSpace(value))
		}
	}
	return false, errors.New("missing enabled state")
}

func loadLogOutputStateFailOpen(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	enabled, err := parseLogOutputState(b)
	if err != nil {
		return true
	}
	return enabled
}
