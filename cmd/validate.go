/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/control"
	"github.com/daeuniverse/dae/pkg/rulesload"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	validateTimings bool

	validateCmd = &cobra.Command{
		Use:   "validate",
		Short: "To validate dae config.",
		Run: func(cmd *cobra.Command, args []string) {
			if cfgFile == "" {
				fmt.Println("Argument \"--config\" or \"-c\" is required but not provided.")
				os.Exit(1)
			}

			// Default validate behaviour is unchanged: success prints nothing,
			// failure prints the error and exits non-zero. Spec
			// docs/superpowers/specs/2026-06-18-dae-rules-load-observability-design.md
			// requires an explicit operator opt-in (either --timings or
			// DAE_RULES_LOAD_TIMINGS=1) before any rules_load events appear,
			// so the existing production workflow stays compatible.
			timingsEnabled := rulesLoadTimingsEnabled(validateTimings, os.Getenv("DAE_RULES_LOAD_TIMINGS"))

			var collector *SummaryCollector
			var stageLog *logrus.Logger
			if timingsEnabled {
				// Stage events and the summary go to stderr so callers piping
				// stdout for a yes/no validation result are not disturbed.
				stageLog = logrus.New()
				stageLog.SetOutput(os.Stderr)
				stageLog.SetLevel(logrus.InfoLevel)
				collector = newSummaryCollector(stageLog, rulesload.LifecycleValidate)
			}

			// Read config from --config cfgFile.
			configLoadStart := time.Now()
			conf, _, loadStats, err := readConfigWithStats(cfgFile)
			if err != nil {
				if collector != nil {
					collector.SetError("config_parse_error")
					collector.RecordConfigStats(loadStats)
					collector.EmitConfigStages(loadStats)
					rulesload.EmitStage(stageLog, rulesload.LifecycleValidate, rulesload.StageReadConfig,
						time.Since(configLoadStart).Milliseconds(), 0, 0, "config_parse_error")
					collector.Emit()
				}
				fmt.Println(err)
				os.Exit(1)
			}
			if collector != nil {
				collector.RecordConfigLoad(time.Since(configLoadStart))
				collector.RecordConfigStats(loadStats)
				rulesload.EmitStage(stageLog, rulesload.LifecycleValidate, rulesload.StageReadConfig,
					time.Since(configLoadStart).Milliseconds(), 0, 0, "")
				collector.EmitConfigStages(loadStats)
			}
			// Run the same FakeIP `routing_outbound(...)` expansion that
			// start/reload runs. Without this, misspelled outbound names,
			// non-fakeip targets, and selector-with-empty-main-routing slip
			// past `dae validate -c` and only fail at reload — violating
			// the operational invariant "validate before reload."
			if err := validateConfigForExpansionWithCollector(conf, collector, stageLog); err != nil {
				if collector != nil {
					collector.SetError("fakeip_auto_expand_error")
					collector.Emit()
				}
				fmt.Println(err)
				os.Exit(1)
			}
			if collector != nil {
				collector.Emit()
			}
		},
	}
)

// validateConfigForExpansion runs the FakeIP `routing_outbound(...)`
// expander against conf using the same outbound namespace that
// newControlPlaneWithMode uses (built-in `direct` and `block`, plus
// every entry in conf.Group). The expansion result is discarded —
// validate is read-only — but any selector error is returned. The
// expander's structured observability events are suppressed because
// validate must not pollute the log stream of the running process or
// of an operator's terminal.
//
// This is the legacy entry point used by validate tests and any caller
// that does not opt into rules-load timings; it preserves the original
// no-side-effects contract. The opt-in `--timings` / DAE_RULES_LOAD_TIMINGS=1
// path uses validateConfigForExpansionWithCollector below.
func validateConfigForExpansion(conf *config.Config) error {
	return validateConfigForExpansionWithCollector(conf, nil, nil)
}

// validateConfigForExpansionWithCollector is the timings-aware variant.
// When collector != nil it records the FakeIP-auto expand stage and the
// derived-rule count into the collector and emits a corresponding
// `rules_load_stage` event via stageLog. When collector == nil it
// preserves the legacy validate behaviour: no log output, no side
// effects beyond returning the parse/expand error.
func validateConfigForExpansionWithCollector(conf *config.Config, collector *SummaryCollector, stageLog *logrus.Logger) error {
	expanderLog := stageLog
	if collector == nil {
		silent := logrus.New()
		silent.SetOutput(io.Discard)
		// PanicLevel suppresses Info/Warn/Error so the `fakeip_event` summary
		// line does not appear during default (no-timings) validation.
		silent.SetLevel(logrus.PanicLevel)
		expanderLog = silent
	}

	expandStart := time.Now()
	expanded, expansionSummary, err := control.ExpandFakeIPRoutingOutboundWithSummary(
		expanderLog,
		conf.Dns.Routing.Request.Rules,
		conf.Routing.Rules,
		staticOutboundExists(conf),
	)
	expandMs := time.Since(expandStart).Milliseconds()
	if err != nil {
		if collector != nil && stageLog != nil {
			rulesload.EmitStage(stageLog, rulesload.LifecycleValidate, rulesload.StageFakeIPAutoExpand,
				expandMs, 0, 0, "fakeip_auto_expand_error")
		}
		return fmt.Errorf("expand routing_outbound DNS selector: %w", err)
	}
	if collector != nil && stageLog != nil {
		rulesload.EmitStage(stageLog, rulesload.LifecycleValidate, rulesload.StageFakeIPAutoExpand,
			expandMs, 0, len(expanded), "")
		collector.RecordStage(rulesload.StageFakeIPAutoExpand, expandMs, 0, len(expanded))
		collector.SetFakeIPAutoDerived(expansionSummary.DerivedRules)
		collector.SetDnsRouting(len(expanded), len(conf.Dns.Routing.Response.Rules))
	}
	return nil
}

// staticOutboundExists returns a predicate matching the static outbound
// namespace determined by config text alone: `direct` and `block` are
// always present, plus every group declared in conf.Group becomes an
// outbound at runtime. Used by both `dae run`/reload (newControlPlaneWithMode)
// and `dae validate` so the two paths cannot drift on which selectors
// they accept.
func staticOutboundExists(conf *config.Config) func(name string) bool {
	names := map[string]struct{}{
		consts.OutboundDirect.String(): {},
		consts.OutboundBlock.String():  {},
	}
	for _, g := range conf.Group {
		names[g.Name] = struct{}{}
	}
	return func(name string) bool {
		_, ok := names[name]
		return ok
	}
}

func init() {
	rootCmd.AddCommand(validateCmd)

	validateCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file")
	validateCmd.PersistentFlags().BoolVar(&validateTimings, "timings", false,
		"emit rules_load_summary / rules_load_stage events on stderr (also: DAE_RULES_LOAD_TIMINGS=1)")
}
