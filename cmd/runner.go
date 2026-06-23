/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"sync"

	"github.com/daeuniverse/dae/config"
	"github.com/sirupsen/logrus"
)

type Runner struct {
	log               *logrus.Logger
	conf              *config.Config
	externGeoDataDirs []string
	collector         *SummaryCollector

	startupEmitOnce sync.Once
}

func newRunner(log *logrus.Logger, conf *config.Config, externGeoDataDirs []string, collector *SummaryCollector) *Runner {
	return &Runner{
		log:               log,
		conf:              conf,
		externGeoDataDirs: externGeoDataDirs,
		collector:         collector,
	}
}

// emitStartupSummaryIfCollector publishes the rules_load_summary event for
// the startup lifecycle. It is a no-op when the Runner has no collector
// (e.g. validate path, tests). Safe to call multiple times per Runner —
// only the first call emits, subsequent calls are silent.
func (r *Runner) emitStartupSummaryIfCollector() {
	if r == nil || r.collector == nil {
		return
	}
	r.startupEmitOnce.Do(r.collector.Emit)
}
