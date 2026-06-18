/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/control"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	validateCmd = &cobra.Command{
		Use:   "validate",
		Short: "To validate dae config.",
		Run: func(cmd *cobra.Command, args []string) {
			if cfgFile == "" {
				fmt.Println("Argument \"--config\" or \"-c\" is required but not provided.")
				os.Exit(1)
			}
			// Read config from --config cfgFile.
			conf, _, err := readConfig(cfgFile)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			// Run the same FakeIP `routing_outbound(...)` expansion that
			// start/reload runs. Without this, misspelled outbound names,
			// non-fakeip targets, and selector-with-empty-main-routing slip
			// past `dae validate -c` and only fail at reload — violating
			// the operational invariant "validate before reload."
			if err := validateConfigForExpansion(conf); err != nil {
				fmt.Println(err)
				os.Exit(1)
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
func validateConfigForExpansion(conf *config.Config) error {
	silentLog := logrus.New()
	silentLog.SetOutput(io.Discard)
	// PanicLevel suppresses Info/Warn/Error so the `fakeip_event` summary
	// line does not appear during validation.
	silentLog.SetLevel(logrus.PanicLevel)

	if _, err := control.ExpandFakeIPRoutingOutbound(
		silentLog,
		conf.Dns.Routing.Request.Rules,
		conf.Routing.Rules,
		staticOutboundExists(conf),
	); err != nil {
		return fmt.Errorf("expand routing_outbound DNS selector: %w", err)
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
}
