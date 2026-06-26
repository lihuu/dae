/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package domain_matcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/pkg/trie"
	"github.com/sirupsen/logrus"
	"github.com/v2rayA/ahocorasick-domain"
)

var ValidDomainChars = trie.NewValidChars([]byte("0123456789abcdefghijklmnopqrstuvwxyz-.^_"))

// BuildStats, when non-nil, receives a per-slot breakdown of an
// AhocorasickSlimtrie.Build run. The struct is opt-in via WithStats and zero
// values are valid (a slot count of zero means the matcher had no patterns of
// that kind). Durations use time.Duration (nanoseconds internally) so they
// compose with the existing rules_load timing pipeline.
//
// Operators read these to decide where the parent stage's wall-clock went:
//   - WallDuration ≈ MaxSlotDuration on either side ⇒ that side's tail slot
//     is the parallel ceiling; algorithmic work (smaller slots, smaller
//     patterns) is the remaining lever.
//   - CpuDuration / WallDuration ≈ effective parallelism ⇒ if this is close
//     to 1 on a multi-core host, the worker cap (or per-side serialization)
//     is the bottleneck.
type BuildStats struct {
	AcSlots             int
	AcPatterns          int
	AcMaxSlotPatterns   int
	AcCpuDuration       time.Duration
	AcMaxSlotDuration   time.Duration
	TrieSlots           int
	TriePatterns        int
	TrieMaxSlotPatterns int
	TrieCpuDuration     time.Duration
	TrieMaxSlotDuration time.Duration
	RegexpSlots         int
	WallDuration        time.Duration
}

type AhocorasickSlimtrie struct {
	log *logrus.Logger

	validAcIndexes     []int
	validTrieIndexes   []int
	validRegexpIndexes []int
	ac                 []*ahocorasick.Matcher
	trie               []*trie.Trie
	regexp             [][]*regexp.Regexp

	toBuildAc   [][][]byte
	toBuildTrie [][]string
	err         error

	stats *BuildStats
}

func NewAhocorasickSlimtrie(log *logrus.Logger, bitLength int) *AhocorasickSlimtrie {
	return &AhocorasickSlimtrie{
		log:         log,
		ac:          make([]*ahocorasick.Matcher, bitLength),
		trie:        make([]*trie.Trie, bitLength),
		regexp:      make([][]*regexp.Regexp, bitLength),
		toBuildAc:   make([][][]byte, bitLength),
		toBuildTrie: make([][]string, bitLength),
	}
}

// WithStats wires an external BuildStats sink. Call before Build. Subsequent
// Build invocations populate the supplied struct with per-slot counts and
// durations; nil disables the feature (zero overhead beyond a nil-check).
func (n *AhocorasickSlimtrie) WithStats(stats *BuildStats) *AhocorasickSlimtrie {
	n.stats = stats
	return n
}
func (n *AhocorasickSlimtrie) AddSet(bitIndex int, patterns []string, typ consts.RoutingDomainKey) {
	if n.err != nil {
		return
	}
nextPattern:
	for _, d := range patterns {
		switch typ {
		case consts.RoutingDomainKey_Full:
			for _, r := range []byte(d) {
				if !ValidDomainChars.IsValidChar(r) {
					n.log.Warnf("DomainMatcher: skip bad full domain: %v: unexpected char: %v", d, string(r))
					continue nextPattern
				}
			}
			n.toBuildTrie[bitIndex] = append(n.toBuildTrie[bitIndex], "^"+d+"$")
		case consts.RoutingDomainKey_Suffix:
			for _, r := range []byte(d) {
				if !ValidDomainChars.IsValidChar(r) {
					n.log.Warnf("DomainMatcher: skip bad suffix domain: %v: unexpected char: %v", d, string(r))
					continue nextPattern
				}
			}
			if strings.HasPrefix(d, ".") {
				// abc.example.com
				n.toBuildTrie[bitIndex] = append(n.toBuildTrie[bitIndex], d+"$")
				// cannot match example.com
			} else {
				// xxx.example.com
				n.toBuildTrie[bitIndex] = append(n.toBuildTrie[bitIndex], "."+d+"$")
				// example.com
				n.toBuildTrie[bitIndex] = append(n.toBuildTrie[bitIndex], "^"+d+"$")
				// cannot match abcexample.com
			}
		case consts.RoutingDomainKey_Keyword:
			// Only use ac automaton for "keyword" matching to save memory.
			n.toBuildAc[bitIndex] = append(n.toBuildAc[bitIndex], []byte(d))
		case consts.RoutingDomainKey_Regex:
			r, err := regexp.Compile(d)
			if err != nil {
				n.err = fmt.Errorf("failed to compile regex: %v", d)
				return
			}
			n.regexp[bitIndex] = append(n.regexp[bitIndex], r)
		default:
			n.err = fmt.Errorf("unknown RoutingDomainKey: %v", typ)
			return
		}
	}
}
func (n *AhocorasickSlimtrie) MatchDomainBitmap(domain string) (bitmap []uint32) {
	N := len(n.ac) / 32
	if len(n.ac)%32 != 0 {
		N++
	}
	bitmap = make([]uint32, N)
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	// Domain should consist of 'a'-'z' and '.' and '-'
	// NOTE: DO NOT VERIFY THE DOMAIN TO MATCH: https://github.com/daeuniverse/dae/issues/528
	// for _, b := range []byte(domain) {
	// 	if !ahocorasick.IsValidChar(b) {
	// 		return bitmap
	// 	}
	// }
	// Suffix matching.
	suffixTrieDomain := ToSuffixTrieString("^" + domain)
	for _, i := range n.validTrieIndexes {
		if bitmap[i/32]&(1<<(i%32)) > 0 {
			// Already matched.
			continue
		}
		if n.trie[i].HasPrefix(suffixTrieDomain) {
			bitmap[i/32] |= 1 << (i % 32)
		}
	}
	// Keyword matching.
	// Add magic chars as head and tail.
	acDomain := "^" + domain + "$"
	for _, i := range n.validAcIndexes {
		if bitmap[i/32]&(1<<(i%32)) > 0 {
			// Already matched.
			continue
		}
		if n.ac[i].Contains([]byte(acDomain)) {
			bitmap[i/32] |= 1 << (i % 32)
		}
	}
	// Regex matching.
	for _, i := range n.validRegexpIndexes {
		if bitmap[i/32]&(1<<(i%32)) > 0 {
			// Already matched.
			continue
		}
		for _, r := range n.regexp[i] {
			if r.MatchString(domain) {
				bitmap[i/32] |= 1 << (i % 32)
				break
			}
		}
	}
	return bitmap
}
func ToSuffixTrieString(s string) string {
	// No need for end char "$".
	b := []byte(strings.TrimSuffix(s, "$"))
	// Reverse.
	half := len(b) / 2
	for i := range half {
		b[i], b[len(b)-i-1] = b[len(b)-i-1], b[i]
	}
	return string(b)
}
func ToSuffixTrieStrings(s []string) []string {
	to := make([]string, len(s))
	for i := range s {
		to[i] = ToSuffixTrieString(s[i])
	}
	return to
}
func (n *AhocorasickSlimtrie) Build() (err error) {
	if n.err != nil {
		return n.err
	}
	n.validAcIndexes = make([]int, 0, len(n.toBuildAc)/8)
	n.validTrieIndexes = make([]int, 0, len(n.toBuildAc)/8)
	n.validRegexpIndexes = make([]int, 0, len(n.toBuildAc)/8)

	// Snapshot per-slot pattern counts before parallel work starts, so stats
	// reflect the input shape independent of build wall-clock noise. Nil-safe.
	var (
		buildStart   time.Time
		acCpuNs      atomic.Int64
		trieCpuNs    atomic.Int64
		acMaxNs      atomic.Int64
		trieMaxNs    atomic.Int64
		acSlots      int
		trieSlots    int
		acPatterns   int
		triePatterns int
		acMaxPats    int
		trieMaxPats  int
	)
	if n.stats != nil {
		buildStart = time.Now()
		for _, patterns := range n.toBuildAc {
			if len(patterns) == 0 {
				continue
			}
			acSlots++
			acPatterns += len(patterns)
			if len(patterns) > acMaxPats {
				acMaxPats = len(patterns)
			}
		}
		for _, patterns := range n.toBuildTrie {
			if len(patterns) == 0 {
				continue
			}
			trieSlots++
			triePatterns += len(patterns)
			if len(patterns) > trieMaxPats {
				trieMaxPats = len(patterns)
			}
		}
	}
	// recordSlot updates atomic accumulators only when stats is set.
	recordSlot := func(cpuTotal *atomic.Int64, cpuMax *atomic.Int64, dur time.Duration) {
		if n.stats == nil {
			return
		}
		ns := dur.Nanoseconds()
		cpuTotal.Add(ns)
		// Lock-free max via CAS loop — slot durations are milliseconds, so
		// contention is negligible and avoids the build-side mutex hot path.
		for {
			prev := cpuMax.Load()
			if ns <= prev {
				break
			}
			if cpuMax.CompareAndSwap(prev, ns) {
				break
			}
		}
	}

	// Build AC automaton and trie in parallel for better performance.
	// Use limited concurrency to avoid overwhelming the system.
	numWorkers := min(
		runtime.GOMAXPROCS(0),
		4, // Limit to 4 workers to balance performance and memory
	)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var buildErr error

	// Build AC automaton in parallel.
	wg.Go(func() {
		sem := make(chan struct{}, numWorkers)
		var innerWg sync.WaitGroup
		for i, toBuild := range n.toBuildAc {
			if len(toBuild) == 0 {
				continue
			}
			innerWg.Add(1)
			sem <- struct{}{}
			go func(idx int, patterns [][]byte) {
				defer func() { <-sem }()
				defer innerWg.Done()
				slotStart := time.Now()
				matcher, err := ahocorasick.NewMatcher(patterns)
				slotDur := time.Since(slotStart)
				if err != nil {
					mu.Lock()
					if buildErr == nil {
						buildErr = err
					}
					mu.Unlock()
					return
				}
				recordSlot(&acCpuNs, &acMaxNs, slotDur)
				mu.Lock()
				n.ac[idx] = matcher
				n.validAcIndexes = append(n.validAcIndexes, idx)
				mu.Unlock()
			}(i, toBuild)
		}
		innerWg.Wait()
	})

	// Build succinct trie in parallel.
	wg.Go(func() {
		sem := make(chan struct{}, numWorkers)
		var innerWg sync.WaitGroup
		for i, toBuild := range n.toBuildTrie {
			if len(toBuild) == 0 {
				continue
			}
			innerWg.Add(1)
			sem <- struct{}{}
			go func(idx int, patterns []string) {
				defer func() { <-sem }()
				defer innerWg.Done()
				slotStart := time.Now()
				transformed := ToSuffixTrieStrings(patterns)
				t, err := trie.NewTrie(transformed, ValidDomainChars)
				slotDur := time.Since(slotStart)
				if err != nil {
					mu.Lock()
					if buildErr == nil {
						buildErr = err
					}
					mu.Unlock()
					return
				}
				recordSlot(&trieCpuNs, &trieMaxNs, slotDur)
				mu.Lock()
				n.trie[idx] = t
				n.validTrieIndexes = append(n.validTrieIndexes, idx)
				mu.Unlock()
			}(i, toBuild)
		}
		innerWg.Wait()
	})

	wg.Wait()

	if buildErr != nil {
		return buildErr
	}

	// Regexp - already compiled during AddSet, just collect indexes.
	for i := range n.regexp {
		if len(n.regexp[i]) == 0 {
			continue
		}
		n.validRegexpIndexes = append(n.validRegexpIndexes, i)
	}

	// Publish stats AFTER both worker pools finished so all atomic counters
	// have settled. Wall-clock wraps the snapshot+parallel build; the small
	// AddSet snapshot loop runs in microseconds on real configs.
	if n.stats != nil {
		n.stats.AcSlots = acSlots
		n.stats.AcPatterns = acPatterns
		n.stats.AcMaxSlotPatterns = acMaxPats
		n.stats.AcCpuDuration = time.Duration(acCpuNs.Load())
		n.stats.AcMaxSlotDuration = time.Duration(acMaxNs.Load())
		n.stats.TrieSlots = trieSlots
		n.stats.TriePatterns = triePatterns
		n.stats.TrieMaxSlotPatterns = trieMaxPats
		n.stats.TrieCpuDuration = time.Duration(trieCpuNs.Load())
		n.stats.TrieMaxSlotDuration = time.Duration(trieMaxNs.Load())
		n.stats.RegexpSlots = len(n.validRegexpIndexes)
		n.stats.WallDuration = time.Since(buildStart)
	}

	// Release unused data.
	n.toBuildAc = nil
	n.toBuildTrie = nil
	return nil
}

// Serialize serializes the trie structures and valid indexes to bytes.
// Note: AC automaton and regex are NOT serialized because they don't support
// serialization. They will be rebuilt on load.
func (n *AhocorasickSlimtrie) Serialize() ([]byte, error) {
	var buf bytes.Buffer

	// Write version
	if err := binary.Write(&buf, binary.LittleEndian, uint32(1)); err != nil {
		return nil, fmt.Errorf("write version: %w", err)
	}

	// Write valid indexes
	if err := binary.Write(&buf, binary.LittleEndian, int32(len(n.validAcIndexes))); err != nil {
		return nil, fmt.Errorf("write ac indexes count: %w", err)
	}
	for _, idx := range n.validAcIndexes {
		if err := binary.Write(&buf, binary.LittleEndian, int32(idx)); err != nil {
			return nil, fmt.Errorf("write ac index: %w", err)
		}
	}

	if err := binary.Write(&buf, binary.LittleEndian, int32(len(n.validTrieIndexes))); err != nil {
		return nil, fmt.Errorf("write trie indexes count: %w", err)
	}
	for _, idx := range n.validTrieIndexes {
		if err := binary.Write(&buf, binary.LittleEndian, int32(idx)); err != nil {
			return nil, fmt.Errorf("write trie index: %w", err)
		}
	}

	if err := binary.Write(&buf, binary.LittleEndian, int32(len(n.validRegexpIndexes))); err != nil {
		return nil, fmt.Errorf("write regexp indexes count: %w", err)
	}
	for _, idx := range n.validRegexpIndexes {
		if err := binary.Write(&buf, binary.LittleEndian, int32(idx)); err != nil {
			return nil, fmt.Errorf("write regexp index: %w", err)
		}
	}

	// Write trie structures
	trieCount := 0
	for _, t := range n.trie {
		if t != nil {
			trieCount++
		}
	}
	if err := binary.Write(&buf, binary.LittleEndian, int32(trieCount)); err != nil {
		return nil, fmt.Errorf("write trie count: %w", err)
	}

	for i, t := range n.trie {
		if t == nil {
			continue
		}
		if err := binary.Write(&buf, binary.LittleEndian, int32(i)); err != nil {
			return nil, fmt.Errorf("write trie index: %w", err)
		}
		data, err := t.Serialize()
		if err != nil {
			return nil, fmt.Errorf("serialize trie %d: %w", i, err)
		}
		if err := binary.Write(&buf, binary.LittleEndian, int32(len(data))); err != nil {
			return nil, fmt.Errorf("write trie %d size: %w", i, err)
		}
		if _, err := buf.Write(data); err != nil {
			return nil, fmt.Errorf("write trie %d data: %w", i, err)
		}
	}

	return buf.Bytes(), nil
}

// Deserialize restores the trie structures and valid indexes from bytes.
// Note: AC automaton and regex are NOT restored. They must be rebuilt separately.
func (n *AhocorasickSlimtrie) Deserialize(data []byte) error {
	buf := bytes.NewReader(data)

	// Read version
	var version uint32
	if err := binary.Read(buf, binary.LittleEndian, &version); err != nil {
		return fmt.Errorf("read version: %w", err)
	}
	if version != 1 {
		return fmt.Errorf("unsupported version: %d", version)
	}

	// Read valid indexes
	var acCount int32
	if err := binary.Read(buf, binary.LittleEndian, &acCount); err != nil {
		return fmt.Errorf("read ac indexes count: %w", err)
	}
	n.validAcIndexes = make([]int, acCount)
	for i := range acCount {
		var idx int32
		if err := binary.Read(buf, binary.LittleEndian, &idx); err != nil {
			return fmt.Errorf("read ac index %d: %w", i, err)
		}
		n.validAcIndexes[i] = int(idx)
	}

	var trieCount int32
	if err := binary.Read(buf, binary.LittleEndian, &trieCount); err != nil {
		return fmt.Errorf("read trie indexes count: %w", err)
	}
	n.validTrieIndexes = make([]int, trieCount)
	for i := range trieCount {
		var idx int32
		if err := binary.Read(buf, binary.LittleEndian, &idx); err != nil {
			return fmt.Errorf("read trie index %d: %w", i, err)
		}
		n.validTrieIndexes[i] = int(idx)
	}

	var regexpCount int32
	if err := binary.Read(buf, binary.LittleEndian, &regexpCount); err != nil {
		return fmt.Errorf("read regexp indexes count: %w", err)
	}
	n.validRegexpIndexes = make([]int, regexpCount)
	for i := range regexpCount {
		var idx int32
		if err := binary.Read(buf, binary.LittleEndian, &idx); err != nil {
			return fmt.Errorf("read regexp index %d: %w", i, err)
		}
		n.validRegexpIndexes[i] = int(idx)
	}

	// Read trie structures
	var storedTrieCount int32
	if err := binary.Read(buf, binary.LittleEndian, &storedTrieCount); err != nil {
		return fmt.Errorf("read trie count: %w", err)
	}

	for i := int32(0); i < storedTrieCount; i++ {
		var idx int32
		if err := binary.Read(buf, binary.LittleEndian, &idx); err != nil {
			return fmt.Errorf("read trie index: %w", err)
		}
		var size int32
		if err := binary.Read(buf, binary.LittleEndian, &size); err != nil {
			return fmt.Errorf("read trie %d size: %w", idx, err)
		}
		trieData := make([]byte, size)
		if _, err := io.ReadFull(buf, trieData); err != nil {
			return fmt.Errorf("read trie %d data: %w", idx, err)
		}
		t, err := trie.Deserialize(trieData)
		if err != nil {
			return fmt.Errorf("deserialize trie %d: %w", idx, err)
		}
		n.trie[idx] = t
	}

	return nil
}

// BuildWithCache attempts to load the matcher from cache first.
// If cache miss, builds from scratch and saves to cache.
// The sourceHash is used as the cache key (typically the hash of geosite.dat).
// If cache is nil or disabled, falls back to regular Build().
func (n *AhocorasickSlimtrie) BuildWithCache(cache *TrieCache, sourceHash []byte) error {
	if cache == nil || !cache.IsEnabled() || len(sourceHash) != sha256.Size {
		return n.Build()
	}

	// Try to load from cache
	tries, err := cache.Load(sourceHash)
	if err != nil {
		n.log.Warnf("Failed to load from cache: %v", err)
	}

	if tries != nil {
		// Cache hit - restore the matcher
		n.validTrieIndexes = make([]int, 0, len(n.toBuildTrie)/8)
		for i, t := range tries {
			if t != nil {
				if i >= len(n.trie) {
					return fmt.Errorf("trie cache slot %d exceeds matcher size %d", i, len(n.trie))
				}
				n.trie[i] = t
				n.validTrieIndexes = append(n.validTrieIndexes, i)
			}
		}
		if err := n.buildNonTrieMatchers(); err != nil {
			return err
		}
		n.log.Debugln("Loaded trie structures from cache")
		return nil
	}

	// Cache miss - build from scratch
	n.log.Debugln("Cache miss, building from scratch")
	if err := n.Build(); err != nil {
		return err
	}

	// Save to cache (only trie structures, not AC automaton)
	cache.Save(sourceHash, n.trie)

	return nil
}

func (n *AhocorasickSlimtrie) buildNonTrieMatchers() error {
	if n.err != nil {
		return n.err
	}

	n.validAcIndexes = make([]int, 0, len(n.toBuildAc)/8)
	n.validRegexpIndexes = make([]int, 0, len(n.toBuildAc)/8)

	var (
		acCpuNs    atomic.Int64
		acMaxNs    atomic.Int64
		acSlots    int
		acPatterns int
		acMaxPats  int
		buildStart time.Time
	)
	if n.stats != nil {
		buildStart = time.Now()
		for _, patterns := range n.toBuildAc {
			if len(patterns) == 0 {
				continue
			}
			acSlots++
			acPatterns += len(patterns)
			if len(patterns) > acMaxPats {
				acMaxPats = len(patterns)
			}
		}
	}

	numWorkers := min(runtime.GOMAXPROCS(0), 4)
	sem := make(chan struct{}, numWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var buildErr error
	recordSlot := func(dur time.Duration) {
		if n.stats == nil {
			return
		}
		ns := dur.Nanoseconds()
		acCpuNs.Add(ns)
		for {
			prev := acMaxNs.Load()
			if ns <= prev {
				break
			}
			if acMaxNs.CompareAndSwap(prev, ns) {
				break
			}
		}
	}

	for i, toBuild := range n.toBuildAc {
		if len(toBuild) == 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, patterns [][]byte) {
			defer func() { <-sem }()
			defer wg.Done()
			slotStart := time.Now()
			matcher, err := ahocorasick.NewMatcher(patterns)
			slotDur := time.Since(slotStart)
			if err != nil {
				mu.Lock()
				if buildErr == nil {
					buildErr = err
				}
				mu.Unlock()
				return
			}
			recordSlot(slotDur)
			mu.Lock()
			n.ac[idx] = matcher
			n.validAcIndexes = append(n.validAcIndexes, idx)
			mu.Unlock()
		}(i, toBuild)
	}
	wg.Wait()
	if buildErr != nil {
		return buildErr
	}

	for i := range n.regexp {
		if len(n.regexp[i]) == 0 {
			continue
		}
		n.validRegexpIndexes = append(n.validRegexpIndexes, i)
	}

	if n.stats != nil {
		n.stats.AcSlots = acSlots
		n.stats.AcPatterns = acPatterns
		n.stats.AcMaxSlotPatterns = acMaxPats
		n.stats.AcCpuDuration = time.Duration(acCpuNs.Load())
		n.stats.AcMaxSlotDuration = time.Duration(acMaxNs.Load())
		n.stats.TrieSlots = len(n.validTrieIndexes)
		n.stats.RegexpSlots = len(n.validRegexpIndexes)
		n.stats.WallDuration = time.Since(buildStart)
	}

	n.toBuildAc = nil
	n.toBuildTrie = nil
	return nil
}
