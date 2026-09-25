// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/daeuniverse/dae/config"
)

type options struct {
	ConfigPath string
	Runs       int
}

type runResult struct {
	Run      int
	Entries  int
	Sections int
	MergeNS  int64
}

type durationSummary struct {
	MinNS    int64
	MedianNS int64
	P95NS    int64
	MaxNS    int64
}

func main() {
	opts := parseFlags()
	if err := run(os.Stdout, opts); err != nil {
		fmt.Fprintf(os.Stderr, "config-load-bench: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.ConfigPath, "config", "", "path to one copied .dae entry config")
	flag.IntVar(&opts.Runs, "runs", 3, "number of config merges in the same process")
	flag.Parse()
	return opts
}

func run(out io.Writer, opts options) error {
	if opts.ConfigPath == "" {
		return fmt.Errorf("set -config to a copied .dae entry config")
	}
	if opts.Runs <= 0 {
		return fmt.Errorf("runs must be greater than zero")
	}

	results := make([]runResult, 0, opts.Runs)
	for i := 1; i <= opts.Runs; i++ {
		result, err := mergeOnce(opts.ConfigPath)
		if err != nil {
			return fmt.Errorf("run %d: %w", i, err)
		}
		result.Run = i
		results = append(results, result)
	}
	fmt.Fprintf(out, "source=%s runs=%d entries=%d sections=%d\n", opts.ConfigPath, opts.Runs, results[0].Entries, results[0].Sections)
	fmt.Fprintln(out, "run entries sections merge_ms")
	for _, result := range results {
		fmt.Fprintf(out, "%d %d %d %.3f\n", result.Run, result.Entries, result.Sections, milliseconds(result.MergeNS))
	}
	summary := summarize(results)
	fmt.Fprintf(out, "summary merge_ms min=%.3f median=%.3f p95=%.3f max=%.3f\n",
		milliseconds(summary.MinNS),
		milliseconds(summary.MedianNS),
		milliseconds(summary.P95NS),
		milliseconds(summary.MaxNS),
	)
	return nil
}

func mergeOnce(path string) (runResult, error) {
	start := time.Now()
	sections, entries, err := config.NewMerger(path).Merge()
	if err != nil {
		return runResult{}, err
	}
	return runResult{
		Entries:  len(entries),
		Sections: len(sections),
		MergeNS:  time.Since(start).Nanoseconds(),
	}, nil
}

func summarize(results []runResult) durationSummary {
	values := make([]int64, 0, len(results))
	for _, result := range results {
		values = append(values, result.MergeNS)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return durationSummary{
		MinNS:    values[0],
		MedianNS: percentile(values, 0.50),
		P95NS:    percentile(values, 0.95),
		MaxNS:    values[len(values)-1],
	}
}

func percentile(sorted []int64, quantile float64) int64 {
	index := int(float64(len(sorted)-1)*quantile + 0.5)
	return sorted[index]
}

func milliseconds(ns int64) float64 {
	return float64(ns) / float64(time.Millisecond)
}
