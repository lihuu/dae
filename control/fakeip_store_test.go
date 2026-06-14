// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2025, daeuniverse Organization <dae@daeuniverse.org>

package control

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"net/netip"
)

func testPrefix29(t *testing.T) netip.Prefix {
	t.Helper()
	prefix, err := netip.ParsePrefix("198.18.0.0/29")
	require.NoError(t, err)
	return prefix
}

func fakeipTestLogger() *logrus.Logger {
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)
	return log
}

func TestFakeIPStoreStableAllocationAndReverseLookup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.db")
	prefix := testPrefix29(t)

	store, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)
	defer store.Close()

	addr1, isNew, err := store.GetOrAllocate("www.google.com")
	require.NoError(t, err)
	require.True(t, isNew)
	require.True(t, prefix.Contains(addr1))

	addr2, isNew, err := store.GetOrAllocate("www.google.com")
	require.NoError(t, err)
	require.False(t, isNew)
	require.Equal(t, addr1, addr2)

	domain, ok := store.LookupDomain(addr1)
	require.True(t, ok)
	require.Equal(t, "www.google.com.", domain)
}

func TestFakeIPStorePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.db")
	prefix := testPrefix29(t)

	store1, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)

	addr1, _, err := store1.GetOrAllocate("example.com")
	require.NoError(t, err)
	store1.Close()

	store2, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)
	defer store2.Close()

	addr2, isNew, err := store2.GetOrAllocate("example.com")
	require.NoError(t, err)
	require.False(t, isNew)
	require.Equal(t, addr1, addr2)

	domain, ok := store2.LookupDomain(addr1)
	require.True(t, ok)
	require.Equal(t, "example.com.", domain)
}

func TestFakeIPStoreConcurrentAllocationIsOneToOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.db")
	prefix, err := netip.ParsePrefix("198.18.0.0/24")
	require.NoError(t, err)

	store, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)
	defer store.Close()

	const numGoroutines = 50
	var wg sync.WaitGroup
	results := make([]netip.Addr, numGoroutines)
	errors := make([]error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			domain := "domain" + string(rune('a'+idx%26)) + string(rune('0'+idx/26)) + ".com"
			addr, _, err := store.GetOrAllocate(domain)
			results[idx] = addr
			errors[idx] = err
		}(i)
	}
	wg.Wait()

	for i := 0; i < numGoroutines; i++ {
		require.NoError(t, errors[i])
	}

	seen := make(map[netip.Addr]bool)
	for _, addr := range results {
		require.False(t, seen[addr], "duplicate IP allocated: %s", addr)
		seen[addr] = true
	}
}

func TestFakeIPStoreNeverAllocatesNetworkOrBroadcast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.db")
	prefix := testPrefix29(t)

	store, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)
	defer store.Close()

	network := prefix.Addr()
	bcast := broadcastAddr(prefix)

	for i := 0; i < 6; i++ {
		domain := "test" + string(rune('a'+i)) + ".com"
		addr, _, err := store.GetOrAllocate(domain)
		require.NoError(t, err)
		require.NotEqual(t, network, addr, "allocated network address")
		require.NotEqual(t, bcast, addr, "allocated broadcast address")
	}
}

func TestFakeIPStoreReturnsExhaustionWithoutReuse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.db")
	prefix := testPrefix29(t)

	store, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)
	defer store.Close()

	for i := 0; i < 6; i++ {
		domain := "domain" + string(rune('a'+i)) + ".com"
		_, _, err := store.GetOrAllocate(domain)
		require.NoError(t, err)
	}

	_, _, err = store.GetOrAllocate("overflow.com")
	require.ErrorIs(t, err, ErrFakeIPPoolExhausted)

	stats := store.Stats()
	require.Equal(t, uint64(6), stats.Allocated)
	require.Equal(t, uint64(0), stats.Remaining)
}

func TestFakeIPStoreWriteFailureDoesNotPublishMapping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.db")
	prefix := testPrefix29(t)

	store, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)
	defer store.Close()

	store.db.Close()

	_, _, err = store.GetOrAllocate("fail.com")
	require.Error(t, err)

	_, ok := store.byDomain["fail.com."]
	require.False(t, ok, "mapping should not be published after write failure")
}

func TestFakeIPStorePrefixMismatchFailsOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.db")

	prefix1, err := netip.ParsePrefix("198.18.0.0/29")
	require.NoError(t, err)

	store1, err := OpenFakeIPStore(path, prefix1, fakeipTestLogger())
	require.NoError(t, err)
	store1.Close()

	prefix2, err := netip.ParsePrefix("198.18.1.0/29")
	require.NoError(t, err)

	_, err = OpenFakeIPStore(path, prefix2, fakeipTestLogger())
	require.ErrorIs(t, err, ErrFakeIPPrefixMismatch)
}

func TestFakeIPStoreCorruptionIsIsolatedAndRebuilt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.db")
	prefix := testPrefix29(t)

	store1, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)
	_, _, err = store1.GetOrAllocate("test.com")
	require.NoError(t, err)
	store1.Close()

	err = os.WriteFile(path, []byte("corrupted data"), 0600)
	require.NoError(t, err)

	store2, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)
	defer store2.Close()

	stats := store2.Stats()
	require.Equal(t, uint64(1), stats.RecoveredDBs)
	require.Equal(t, uint64(0), stats.Allocated)
}

func TestFakeIPStoreIterationReturnsEveryMapping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.db")
	prefix := testPrefix29(t)

	store, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)
	defer store.Close()

	domains := []string{"a.com", "b.com", "c.com"}
	expected := make(map[string]netip.Addr)

	for _, d := range domains {
		addr, _, err := store.GetOrAllocate(d)
		require.NoError(t, err)
		expected[d+"."] = addr
	}

	found := make(map[string]netip.Addr)
	err = store.Range(func(domain string, ip netip.Addr) error {
		found[domain] = ip
		return nil
	})
	require.NoError(t, err)

	require.Equal(t, expected, found)
}

func TestFakeIPStoreStats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.db")
	prefix := testPrefix29(t)

	store, err := OpenFakeIPStore(path, prefix, fakeipTestLogger())
	require.NoError(t, err)
	defer store.Close()

	stats := store.Stats()
	require.Equal(t, prefix, stats.Prefix)
	require.Equal(t, path, stats.StorePath)
	require.Equal(t, uint64(6), stats.Capacity)
	require.Equal(t, uint64(0), stats.Allocated)
	require.Equal(t, uint64(6), stats.Remaining)

	store.GetOrAllocate("test.com")
	stats = store.Stats()
	require.Equal(t, uint64(1), stats.Allocated)
	require.Equal(t, uint64(5), stats.Remaining)
}
