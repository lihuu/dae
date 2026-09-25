// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/antlr/antlr4/runtime/Go/antlr/v4"
	"github.com/daeuniverse/dae-config-dist/go/dae_config"
	"github.com/daeuniverse/dae/pkg/config_parser"
)

type options struct {
	ConfigPath            string
	SyntheticRoutingRules int
	Runs                  int
	Mode                  string
	Walk                  bool
	Format                string
	Entry                 string
}

type runResult struct {
	Run           int   `json:"run"`
	TokenCount    int   `json:"token_count"`
	SectionCount  int   `json:"section_count,omitempty"`
	TokenizeNS    int64 `json:"tokenize_ns"`
	ParserInitNS  int64 `json:"parser_init_ns"`
	ParserStartNS int64 `json:"parser_start_ns"`
	TreeWalkNS    int64 `json:"tree_walk_ns,omitempty"`
	TotalNS       int64 `json:"total_ns"`
}

type durationSummary struct {
	MinNS    int64 `json:"min_ns"`
	MedianNS int64 `json:"median_ns"`
	P95NS    int64 `json:"p95_ns"`
	MaxNS    int64 `json:"max_ns"`
}

type report struct {
	Source      string                     `json:"source"`
	Bytes       int                        `json:"bytes"`
	Mode        string                     `json:"mode"`
	Entry       string                     `json:"entry"`
	WalkEnabled bool                       `json:"walk_enabled"`
	Runs        []runResult                `json:"runs"`
	Summary     map[string]durationSummary `json:"summary"`
}

func main() {
	opts := parseFlags()
	if err := run(os.Stdout, opts); err != nil {
		fmt.Fprintf(os.Stderr, "config-parser-bench: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.ConfigPath, "config", "", "path to one .dae file")
	flag.IntVar(&opts.SyntheticRoutingRules, "synthetic-routing-rules", 0, "generate a routing section with N rules instead of reading a file")
	flag.IntVar(&opts.Runs, "runs", 5, "number of parses in the same process; run 1 is coldest and later runs reuse ANTLR DFA state")
	flag.StringVar(&opts.Mode, "mode", "ll", "ANTLR prediction mode: ll, sll, or sll-fallback")
	flag.BoolVar(&opts.Walk, "walk", true, "walk the parse tree and report its duration")
	flag.StringVar(&opts.Format, "format", "text", "output format: text or json")
	flag.StringVar(&opts.Entry, "entry", "start", "parser entry: start or expressions")
	flag.Parse()
	return opts
}

func run(out io.Writer, opts options) error {
	if opts.Runs <= 0 {
		return fmt.Errorf("runs must be greater than zero")
	}
	if opts.ConfigPath == "" && opts.SyntheticRoutingRules == 0 {
		return fmt.Errorf("set exactly one of -config or -synthetic-routing-rules")
	}
	if opts.ConfigPath != "" && opts.SyntheticRoutingRules != 0 {
		return fmt.Errorf("-config and -synthetic-routing-rules are mutually exclusive")
	}
	if opts.SyntheticRoutingRules < 0 {
		return fmt.Errorf("synthetic-routing-rules cannot be negative")
	}
	if opts.Format != "text" && opts.Format != "json" {
		return fmt.Errorf("unsupported format %q", opts.Format)
	}
	if opts.Entry == "" {
		opts.Entry = "start"
	}
	if opts.Entry != "start" && opts.Entry != "expressions" {
		return fmt.Errorf("unsupported entry %q: use start or expressions", opts.Entry)
	}

	mode, err := predictionMode(opts.Mode)
	if err != nil {
		return err
	}
	fallback := strings.EqualFold(opts.Mode, "sll-fallback")
	source, input, err := loadInput(opts)
	if err != nil {
		return err
	}

	rep := report{
		Source:      source,
		Bytes:       len(input),
		Mode:        strings.ToLower(opts.Mode),
		Entry:       opts.Entry,
		WalkEnabled: opts.Walk,
		Runs:        make([]runResult, 0, opts.Runs),
	}
	for i := 1; i <= opts.Runs; i++ {
		result, err := parseOnce(input, mode, opts.Walk, fallback, opts.Entry)
		if err != nil {
			return fmt.Errorf("run %d: %w", i, err)
		}
		result.Run = i
		rep.Runs = append(rep.Runs, result)
	}
	rep.Summary = summarize(rep.Runs)

	if opts.Format == "json" {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(rep)
	}
	writeText(out, rep)
	return nil
}

func loadInput(opts options) (source string, input string, err error) {
	if opts.ConfigPath != "" {
		raw, err := os.ReadFile(opts.ConfigPath)
		if err != nil {
			return "", "", fmt.Errorf("read %s: %w", opts.ConfigPath, err)
		}
		return opts.ConfigPath, string(raw), nil
	}

	var b strings.Builder
	b.Grow(opts.SyntheticRoutingRules * 48)
	b.WriteString("routing {\n")
	for i := 0; i < opts.SyntheticRoutingRules; i++ {
		fmt.Fprintf(&b, "domain(suffix: example%d.test) -> proxy\n", i)
	}
	b.WriteString("}\n")
	return fmt.Sprintf("synthetic:routing_rules=%d", opts.SyntheticRoutingRules), b.String(), nil
}

func predictionMode(name string) (int, error) {
	switch strings.ToLower(name) {
	case "ll":
		return antlr.PredictionModeLL, nil
	case "sll":
		return antlr.PredictionModeSLL, nil
	case "sll-fallback":
		return antlr.PredictionModeSLL, nil
	default:
		return 0, fmt.Errorf("unsupported mode %q: use ll, sll, or sll-fallback", name)
	}
}

func parseOnce(input string, mode int, walk bool, fallback bool, entry string) (runResult, error) {
	if entry == "" {
		entry = "start"
	}
	totalStart := time.Now()
	result, err := parseOnceAttempt(input, mode, walk, entry)
	if err == nil || !fallback {
		result.TotalNS = time.Since(totalStart).Nanoseconds()
		return result, err
	}
	llResult, llErr := parseOnceAttempt(input, antlr.PredictionModeLL, walk, entry)
	llResult.TokenizeNS += result.TokenizeNS
	llResult.ParserInitNS += result.ParserInitNS
	llResult.ParserStartNS += result.ParserStartNS
	llResult.TreeWalkNS += result.TreeWalkNS
	llResult.TotalNS = time.Since(totalStart).Nanoseconds()
	return llResult, llErr
}

func parseOnceAttempt(input string, mode int, walk bool, entry string) (runResult, error) {
	totalStart := time.Now()
	errorListener := config_parser.NewConsoleErrorListener()

	tokenizeStart := time.Now()
	lexer := dae_config.Newdae_configLexer(antlr.NewInputStream(input))
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(errorListener)
	tokenStream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	tokenStream.Fill()
	tokenizeDuration := time.Since(tokenizeStart)
	tokenCount := len(tokenStream.GetAllTokens())
	tokenStream.Seek(0)

	parserInitStart := time.Now()
	parser := dae_config.Newdae_configParser(tokenStream)
	parser.RemoveErrorListeners()
	parser.AddErrorListener(errorListener)
	parser.BuildParseTrees = true
	parser.GetInterpreter().SetPredictionMode(mode)
	parserInitDuration := time.Since(parserInitStart)

	parserStart := time.Now()
	tree, err := parseEntry(parser, tokenStream, entry)
	parserStartDuration := time.Since(parserStart)
	if err != nil {
		return runResult{}, err
	}
	if errorListener.ErrorBuilder.Len() != 0 {
		return runResult{}, errors.New(errorListener.ErrorBuilder.String())
	}

	var treeWalkDuration time.Duration
	var sectionCount int
	if walk {
		walker := config_parser.NewWalker(parser)
		treeWalkStart := time.Now()
		antlr.ParseTreeWalkerDefault.Walk(walker, tree)
		treeWalkDuration = time.Since(treeWalkStart)
		sectionCount = len(walker.Sections)
		if errorListener.ErrorBuilder.Len() != 0 {
			return runResult{}, errors.New(errorListener.ErrorBuilder.String())
		}
	}

	return runResult{
		TokenCount:    tokenCount,
		SectionCount:  sectionCount,
		TokenizeNS:    tokenizeDuration.Nanoseconds(),
		ParserInitNS:  parserInitDuration.Nanoseconds(),
		ParserStartNS: parserStartDuration.Nanoseconds(),
		TreeWalkNS:    treeWalkDuration.Nanoseconds(),
		TotalNS:       time.Since(totalStart).Nanoseconds(),
	}, nil
}

type entryParser interface {
	Start() dae_config.IStartContext
	Expression() dae_config.IExpressionContext
}

func parseEntry(parser entryParser, tokenStream *antlr.CommonTokenStream, entry string) (antlr.Tree, error) {
	switch entry {
	case "start":
		return parser.Start(), nil
	case "expressions":
		return parseExpressionsEntry(parser, tokenStream), nil
	default:
		return nil, fmt.Errorf("unsupported entry %q", entry)
	}
}

func parseExpressionsEntry(parser entryParser, tokenStream *antlr.CommonTokenStream) antlr.Tree {
	root := antlr.NewBaseParserRuleContext(nil, -1)
	for tokenStream.LA(1) != antlr.TokenEOF {
		root.AddChild(parser.Expression())
	}
	return root
}

func summarize(runs []runResult) map[string]durationSummary {
	fields := map[string][]int64{
		"tokenize":     {},
		"parser_init":  {},
		"parser_start": {},
		"tree_walk":    {},
		"total":        {},
	}
	for _, result := range runs {
		fields["tokenize"] = append(fields["tokenize"], result.TokenizeNS)
		fields["parser_init"] = append(fields["parser_init"], result.ParserInitNS)
		fields["parser_start"] = append(fields["parser_start"], result.ParserStartNS)
		fields["tree_walk"] = append(fields["tree_walk"], result.TreeWalkNS)
		fields["total"] = append(fields["total"], result.TotalNS)
	}

	summary := make(map[string]durationSummary, len(fields))
	for name, values := range fields {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		summary[name] = durationSummary{
			MinNS:    values[0],
			MedianNS: percentile(values, 0.50),
			P95NS:    percentile(values, 0.95),
			MaxNS:    values[len(values)-1],
		}
	}
	return summary
}

func percentile(sorted []int64, quantile float64) int64 {
	index := int(float64(len(sorted)-1)*quantile + 0.5)
	return sorted[index]
}

func writeText(out io.Writer, rep report) {
	fmt.Fprintf(out, "source=%s bytes=%d mode=%s entry=%s walk=%t runs=%d\n",
		rep.Source, rep.Bytes, rep.Mode, rep.Entry, rep.WalkEnabled, len(rep.Runs))
	fmt.Fprintln(out, "run tokens sections tokenize_ms parser_init_ms parser_start_ms tree_walk_ms total_ms")
	for _, result := range rep.Runs {
		fmt.Fprintf(out, "%d %d %d %.3f %.3f %.3f %.3f %.3f\n",
			result.Run,
			result.TokenCount,
			result.SectionCount,
			milliseconds(result.TokenizeNS),
			milliseconds(result.ParserInitNS),
			milliseconds(result.ParserStartNS),
			milliseconds(result.TreeWalkNS),
			milliseconds(result.TotalNS),
		)
	}
	for _, name := range []string{"tokenize", "parser_init", "parser_start", "tree_walk", "total"} {
		value := rep.Summary[name]
		fmt.Fprintf(out, "summary phase=%s min_ms=%.3f median_ms=%.3f p95_ms=%.3f max_ms=%.3f\n",
			name,
			milliseconds(value.MinNS),
			milliseconds(value.MedianNS),
			milliseconds(value.P95NS),
			milliseconds(value.MaxNS),
		)
	}
}

func milliseconds(ns int64) float64 {
	return float64(ns) / float64(time.Millisecond)
}
