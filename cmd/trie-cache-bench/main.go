/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@daeuniverse.org>
 */

package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/daeuniverse/dae/component/routing/domain_matcher"
	"github.com/daeuniverse/dae/pkg/geodata"
	"github.com/daeuniverse/dae/pkg/trie"
	"github.com/sirupsen/logrus"
)

func main() {
	geositePath := flag.String("geosite", "", "Path to geosite.dat file")
	code := flag.String("code", "cn", "Geosite code to extract (default: cn)")
	flag.Parse()

	if *geositePath == "" {
		fmt.Fprintf(os.Stderr, "Error: -geosite flag is required\n")
		flag.Usage()
		os.Exit(1)
	}

	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)

	fmt.Printf("Loading geosite data from %s (code: %s)...\n", *geositePath, *code)
	geosite, err := geodata.UnmarshalGeoSite(log, *geositePath, *code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading geosite: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Loaded %d domains\n", len(geosite.Domain))

	// Convert domains to suffix patterns (like DAE does with @geosite:cn)
	var patterns []string
	for _, domain := range geosite.Domain {
		// Skip regex patterns for now (we'll handle them separately if needed)
		if domain.Type == geodata.Domain_Regex {
			continue
		}

		// Convert to suffix pattern
		domainName := domain.Value
		switch domain.Type {
		case geodata.Domain_Plain:
			// For plain, we want suffix matching
			patterns = append(patterns, "."+domainName)
			patterns = append(patterns, domainName)
		case geodata.Domain_RootDomain:
			// Root domain: suffix matching
			patterns = append(patterns, "."+domainName)
			patterns = append(patterns, domainName)
		case geodata.Domain_Full:
			// Full match
			patterns = append(patterns, domainName)
		case geodata.Domain_Regex:
			// Skip regex for now
			continue
		}
	}

	fmt.Printf("Generated %d patterns\n", len(patterns))

	// Deduplicate patterns
	patternMap := make(map[string]bool)
	var uniquePatterns []string
	for _, p := range patterns {
		if !patternMap[p] {
			patternMap[p] = true
			uniquePatterns = append(uniquePatterns, p)
		}
	}
	patterns = uniquePatterns
	fmt.Printf("After dedup: %d unique patterns\n", len(patterns))

	// Build trie from scratch
	fmt.Println("\n=== Building trie from scratch ===")
	buildStart := time.Now()
	t, err := trie.NewTrie(patterns, domain_matcher.ValidDomainChars)
	buildDur := time.Since(buildStart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building trie: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Build time: %v\n", buildDur)

	// Serialize trie
	fmt.Println("\n=== Serializing trie ===")
	serializeStart := time.Now()
	data, err := t.Serialize()
	serializeDur := time.Since(serializeStart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error serializing trie: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Serialize time: %v\n", serializeDur)
	fmt.Printf("Serialized size: %d bytes (%.2f MB)\n", len(data), float64(len(data))/(1024*1024))

	// Deserialize trie
	fmt.Println("\n=== Deserializing trie ===")
	deserializeStart := time.Now()
	t2, err := trie.Deserialize(data)
	deserializeDur := time.Since(deserializeStart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error deserializing trie: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Deserialize time: %v\n", deserializeDur)

	// Verify correctness by testing a few lookups
	fmt.Println("\n=== Verifying correctness ===")
	testDomains := []string{
		"baidu.com",
		"www.baidu.com",
		"google.com",
		"example.cn",
		"test.edu.cn",
	}
	for _, domain := range testDomains {
		// Add leading dot for suffix matching
		result1 := t.HasPrefix("." + domain)
		result2 := t2.HasPrefix("." + domain)
		if result1 != result2 {
			fmt.Printf("MISMATCH for %s: original=%v, deserialized=%v\n", domain, result1, result2)
		} else {
			fmt.Printf("OK: %s -> %v\n", domain, result1)
		}
	}

	// Summary
	fmt.Println("\n=== Summary ===")
	fmt.Printf("Patterns: %d\n", len(patterns))
	fmt.Printf("Build time: %v\n", buildDur)
	fmt.Printf("Serialize time: %v\n", serializeDur)
	fmt.Printf("Deserialize time: %v\n", deserializeDur)
	fmt.Printf("Serialized size: %.2f MB\n", float64(len(data))/(1024*1024))

	// Calculate speedup
	if deserializeDur > 0 {
		speedup := float64(buildDur) / float64(deserializeDur)
		fmt.Printf("\nSpeedup (build/deserialize): %.2fx\n", speedup)
		if speedup > 1.0 {
			fmt.Printf("✓ Deserialization is FASTER than building from scratch\n")
		} else {
			fmt.Printf("✗ Deserialization is SLOWER than building from scratch\n")
		}
	}
}
