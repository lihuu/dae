/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config_parser

import (
	"fmt"
	"time"

	"github.com/antlr/antlr4/runtime/Go/antlr/v4"
	"github.com/daeuniverse/dae-config-dist/go/dae_config"
)

type ParseProfile struct {
	Bytes        int
	Tokens       int
	Sections     int
	Items        int
	RoutingRules int
	Functions    int
	Params       int

	InputStreamDuration  time.Duration
	LexerCreateDuration  time.Duration
	TokenFillDuration    time.Duration
	ParserCreateDuration time.Duration
	ParseDuration        time.Duration
	WalkDuration         time.Duration
}

func parseWithProfile(in string) (sections []*Section, profile ParseProfile, err error) {
	profile.Bytes = len(in)
	errorListener := NewConsoleErrorListener()

	inputStart := time.Now()
	stream := antlr.NewInputStream(in)
	profile.InputStreamDuration = time.Since(inputStart)

	lexerStart := time.Now()
	lexer := dae_config.Newdae_configLexer(stream)
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(errorListener)
	profile.LexerCreateDuration = time.Since(lexerStart)

	input := antlr.NewCommonTokenStream(lexer, 0)
	tokenFillStart := time.Now()
	input.Fill()
	profile.TokenFillDuration = time.Since(tokenFillStart)
	profile.Tokens = input.Size()
	input.Seek(0)

	parserStart := time.Now()
	parser := dae_config.Newdae_configParser(input)
	parser.RemoveErrorListeners()
	parser.AddErrorListener(errorListener)
	parser.BuildParseTrees = true
	profile.ParserCreateDuration = time.Since(parserStart)

	parseStart := time.Now()
	tree := parser.Start()
	profile.ParseDuration = time.Since(parseStart)

	walker := NewWalker(parser)
	walkStart := time.Now()
	antlr.ParseTreeWalkerDefault.Walk(walker, tree)
	profile.WalkDuration = time.Since(walkStart)
	if errorListener.ErrorBuilder.Len() != 0 {
		return nil, profile, fmt.Errorf("%v", errorListener.ErrorBuilder.String())
	}

	profile.Sections = len(walker.Sections)
	profile.Items, profile.RoutingRules, profile.Functions, profile.Params = countProfileItems(walker.Sections)
	return walker.Sections, profile, nil
}

func countProfileItems(sections []*Section) (items int, routingRules int, functions int, params int) {
	for _, section := range sections {
		sectionItems, sectionRoutingRules, sectionFunctions, sectionParams := countSectionProfileItems(section)
		items += sectionItems
		routingRules += sectionRoutingRules
		functions += sectionFunctions
		params += sectionParams
	}
	return items, routingRules, functions, params
}

func countSectionProfileItems(section *Section) (items int, routingRules int, functions int, params int) {
	for _, item := range section.Items {
		items++
		switch v := item.Value.(type) {
		case *Section:
			childItems, childRoutingRules, childFunctions, childParams := countSectionProfileItems(v)
			items += childItems
			routingRules += childRoutingRules
			functions += childFunctions
			params += childParams
		case *RoutingRule:
			routingRules++
			functions += len(v.AndFunctions) + 1
			for _, f := range v.AndFunctions {
				params += len(f.Params)
			}
			params += len(v.Outbound.Params)
		case *Param:
			params++
			for _, f := range v.AndFunctions {
				functions++
				params += len(f.Params)
			}
		}
	}
	return items, routingRules, functions, params
}
