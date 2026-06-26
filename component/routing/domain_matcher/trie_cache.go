/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@daeuniverse.org>
 */

package domain_matcher

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"

	"github.com/daeuniverse/dae/pkg/trie"
	"github.com/sirupsen/logrus"
)

const (
	// TrieCacheVersion is the cache format version. Increment when format changes.
	TrieCacheVersion uint32 = 1
)

// TrieCache manages persistent caching of compiled trie structures.
// Cache is invalidated when the source file hash changes.
type TrieCache struct {
	path    string
	log     *logrus.Logger
	enabled bool
}

// NewTrieCache creates a new cache manager.
// If path is empty or enabled is false, caching is disabled.
func NewTrieCache(log *logrus.Logger, path string, enabled bool) *TrieCache {
	return &TrieCache{
		path:    path,
		log:     log,
		enabled: enabled && path != "",
	}
}

// IsEnabled returns whether caching is enabled.
func (c *TrieCache) IsEnabled() bool {
	return c.enabled
}

// Load attempts to load cached trie structures for the given source hash.
// Returns nil, nil if cache miss or error (caller should rebuild).
// Returns the trie array and nil on success.
func (c *TrieCache) Load(sourceHash []byte) ([]*trie.Trie, error) {
	if !c.enabled {
		return nil, nil
	}

	file, err := os.Open(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			c.log.Debugln("Trie cache miss: file does not exist")
			return nil, nil
		}
		c.log.Warnf("Trie cache: failed to open %s: %v", c.path, err)
		return nil, nil
	}
	defer file.Close()

	// Read and verify version
	var version uint32
	if err := binary.Read(file, binary.LittleEndian, &version); err != nil {
		c.log.Warnf("Trie cache: failed to read version: %v", err)
		return nil, nil
	}
	if version != TrieCacheVersion {
		c.log.Warnf("Trie cache: version mismatch (have %d, want %d)", version, TrieCacheVersion)
		return nil, nil
	}

	// Read and verify hash
	cachedHash := make([]byte, 32)
	if _, err := io.ReadFull(file, cachedHash); err != nil {
		c.log.Warnf("Trie cache: failed to read hash: %v", err)
		return nil, nil
	}
	if !bytesEqual(cachedHash, sourceHash) {
		c.log.Debugln("Trie cache miss: source hash mismatch")
		return nil, nil
	}

	// Read trie count
	var trieCount uint32
	if err := binary.Read(file, binary.LittleEndian, &trieCount); err != nil {
		c.log.Warnf("Trie cache: failed to read trie count: %v", err)
		return nil, nil
	}

	// Read each trie
	tries := make([]*trie.Trie, trieCount)
	for i := uint32(0); i < trieCount; i++ {
		// Read trie size
		var size uint32
		if err := binary.Read(file, binary.LittleEndian, &size); err != nil {
			c.log.Warnf("Trie cache: failed to read trie %d size: %v", i, err)
			return nil, nil
		}

		// Read trie data
		data := make([]byte, size)
		if _, err := io.ReadFull(file, data); err != nil {
			c.log.Warnf("Trie cache: failed to read trie %d data: %v", i, err)
			return nil, nil
		}
		if size == 0 {
			tries[i] = nil
			continue
		}

		// Deserialize trie
		t, err := trie.Deserialize(data)
		if err != nil {
			c.log.Warnf("Trie cache: failed to deserialize trie %d: %v", i, err)
			return nil, nil
		}
		tries[i] = t
	}

	c.log.Infof("Trie cache hit: loaded %d tries from %s", trieCount, c.path)
	return tries, nil
}

// Save writes the trie structures to cache with the given source hash.
// Errors are logged but do not affect the main build process.
func (c *TrieCache) Save(sourceHash []byte, tries []*trie.Trie) {
	if !c.enabled {
		return
	}

	// Ensure parent directory exists
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		c.log.Warnf("Trie cache: failed to create directory %s: %v", dir, err)
		return
	}

	// Write to temporary file first
	tmpPath := c.path + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		c.log.Warnf("Trie cache: failed to create temp file %s: %v", tmpPath, err)
		return
	}
	defer func() {
		file.Close()
		// Clean up temp file on error
		if err != nil {
			os.Remove(tmpPath)
		}
	}()

	// Write version
	if err = binary.Write(file, binary.LittleEndian, TrieCacheVersion); err != nil {
		c.log.Warnf("Trie cache: failed to write version: %v", err)
		return
	}

	// Write hash
	if _, err = file.Write(sourceHash); err != nil {
		c.log.Warnf("Trie cache: failed to write hash: %v", err)
		return
	}

	// Write trie count
	if err = binary.Write(file, binary.LittleEndian, uint32(len(tries))); err != nil {
		c.log.Warnf("Trie cache: failed to write trie count: %v", err)
		return
	}

	// Write each trie
	for i, t := range tries {
		if t == nil {
			// Write zero size for nil tries
			if err = binary.Write(file, binary.LittleEndian, uint32(0)); err != nil {
				c.log.Warnf("Trie cache: failed to write trie %d size: %v", i, err)
				return
			}
			continue
		}

		// Serialize trie
		data, err := t.Serialize()
		if err != nil {
			c.log.Warnf("Trie cache: failed to serialize trie %d: %v", i, err)
			return
		}

		// Write size
		if err = binary.Write(file, binary.LittleEndian, uint32(len(data))); err != nil {
			c.log.Warnf("Trie cache: failed to write trie %d size: %v", i, err)
			return
		}

		// Write data
		if _, err = file.Write(data); err != nil {
			c.log.Warnf("Trie cache: failed to write trie %d data: %v", i, err)
			return
		}
	}

	// Close file before rename
	if err = file.Close(); err != nil {
		c.log.Warnf("Trie cache: failed to close temp file: %v", err)
		return
	}

	// Atomically replace the cache file
	if err = os.Rename(tmpPath, c.path); err != nil {
		c.log.Warnf("Trie cache: failed to rename temp file: %v", err)
		return
	}

	c.log.Infof("Trie cache saved: wrote %d tries to %s", len(tries), c.path)
}

// Invalidate removes the cache file.
func (c *TrieCache) Invalidate() {
	if !c.enabled {
		return
	}
	if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
		c.log.Warnf("Trie cache: failed to remove %s: %v", c.path, err)
	}
}

// ComputeHash computes SHA256 hash of the given data.
func ComputeHash(data []byte) []byte {
	hash := sha256.Sum256(data)
	return hash[:]
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
