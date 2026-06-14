// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2025, daeuniverse Organization <dae@daeuniverse.org>

package control

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
)

var (
	ErrFakeIPPoolExhausted  = errors.New("fakeip IPv4 pool exhausted")
	ErrUnknownFakeIP        = errors.New("unknown fakeip address")
	ErrFakeIPPrefixMismatch = errors.New("fakeip store prefix does not match configuration")
)

var (
	bucketMetadata    = []byte("metadata")
	bucketDomainToIP  = []byte("domain_to_ip")
	bucketIPToDomain  = []byte("ip_to_domain")
	keySchemaVersion  = []byte("schema_version")
	keyPrefix         = []byte("prefix")
	keyNext           = []byte("next")
	schemaVersionValue = []byte("1")
)

type FakeIPStats struct {
	Prefix       netip.Prefix
	Capacity     uint64
	Allocated    uint64
	Remaining    uint64
	Next         netip.Addr
	StorePath    string
	RecoveredDBs uint64
}

type FakeIPStore struct {
	mu           sync.RWMutex
	db           *bolt.DB
	prefix       netip.Prefix
	path         string
	next         uint32
	first        uint32
	last         uint32
	byDomain     map[string]netip.Addr
	byIP         map[netip.Addr]string
	stats        atomic.Pointer[FakeIPStats]
	log          *logrus.Logger
	recoveredDBs uint64
}

func OpenFakeIPStore(path string, prefix netip.Prefix, log *logrus.Logger) (*FakeIPStore, error) {
	if !prefix.IsValid() || prefix.Addr().Is6() {
		return nil, fmt.Errorf("fakeip store requires a valid IPv4 prefix")
	}

	network := prefix.Addr()
	bcast := broadcastAddr(prefix)
	first := addrToUint(network) + 1
	last := addrToUint(bcast) - 1

	if first > last {
		return nil, fmt.Errorf("fakeip prefix %s has no allocatable IPv4 addresses", prefix)
	}

	s := &FakeIPStore{
		prefix:   prefix,
		path:     path,
		first:    first,
		last:     last,
		byDomain: make(map[string]netip.Addr),
		byIP:     make(map[netip.Addr]string),
		log:      log,
	}

	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		if isCorruptionError(err) {
			if recoverErr := s.isolateAndRecreate(path); recoverErr != nil {
				return nil, fmt.Errorf("corruption recovery failed: %w (original: %v)", recoverErr, err)
			}
			db, err = bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
			if err != nil {
				return nil, fmt.Errorf("reopen after corruption recovery: %w", err)
			}
		} else {
			return nil, fmt.Errorf("open fakeip store %s: %w", path, err)
		}
	}
	s.db = db

	if err := s.initOrRestore(); err != nil {
		db.Close()
		return nil, err
	}

	s.updateStats()
	return s, nil
}

func isCorruptionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid free list") ||
		strings.Contains(msg, "checksum error") ||
		strings.Contains(msg, "meta page error") ||
		strings.Contains(msg, "invalid page") ||
		strings.Contains(msg, "txid too high") ||
		strings.Contains(msg, "invalid database")
}

func (s *FakeIPStore) isolateAndRecreate(path string) error {
	ts := time.Now().UTC().Format("20060102T150405Z")
	corruptPath := path + ".corrupt-" + ts

	if err := os.Rename(path, corruptPath); err != nil {
		return fmt.Errorf("rename corrupt db to %s: %w", corruptPath, err)
	}

	s.log.WithFields(logrus.Fields{
		"original":    path,
		"corruptPath": corruptPath,
	}).Error("FakeIP store corruption detected; isolated corrupt database and creating fresh store")

	s.recoveredDBs = 1
	return nil
}

func (s *FakeIPStore) initOrRestore() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(bucketMetadata)
		if err != nil {
			return fmt.Errorf("create metadata bucket: %w", err)
		}
		if _, err := tx.CreateBucketIfNotExists(bucketDomainToIP); err != nil {
			return fmt.Errorf("create domain_to_ip bucket: %w", err)
		}
		if _, err := tx.CreateBucketIfNotExists(bucketIPToDomain); err != nil {
			return fmt.Errorf("create ip_to_domain bucket: %w", err)
		}

		storedVersion := meta.Get(keySchemaVersion)
		if storedVersion == nil {
			s.next = s.first
			if err := meta.Put(keySchemaVersion, schemaVersionValue); err != nil {
				return err
			}
			if err := meta.Put(keyPrefix, []byte(s.prefix.String())); err != nil {
				return err
			}
			return meta.Put(keyNext, uint32ToBytes(s.next))
		}

		if string(storedVersion) != string(schemaVersionValue) {
			return fmt.Errorf("unsupported fakeip store schema version: %s", storedVersion)
		}

		storedPrefix := string(meta.Get(keyPrefix))
		if storedPrefix != s.prefix.String() {
			return fmt.Errorf("%w: stored=%s configured=%s", ErrFakeIPPrefixMismatch, storedPrefix, s.prefix.String())
		}

		nextBytes := meta.Get(keyNext)
		if nextBytes != nil && len(nextBytes) == 4 {
			s.next = binary.BigEndian.Uint32(nextBytes)
		} else {
			s.next = s.first
		}

		d2p := tx.Bucket(bucketDomainToIP)
		p2d := tx.Bucket(bucketIPToDomain)

		return d2p.ForEach(func(k, v []byte) error {
			domain := string(k)
			if len(v) != 4 {
				return fmt.Errorf("corrupt domain_to_ip entry for %s: expected 4 bytes, got %d", domain, len(v))
			}
			addr := uint32ToAddr(binary.BigEndian.Uint32(v))
			if !s.prefix.Contains(addr) {
				return fmt.Errorf("corrupt domain_to_ip entry: %s -> %s outside prefix %s", domain, addr, s.prefix)
			}
			s.byDomain[domain] = addr
			s.byIP[addr] = domain

			reverseDomain := p2d.Get(v)
			if reverseDomain == nil || string(reverseDomain) != domain {
				return fmt.Errorf("bijective inconsistency: domain_to_ip has %s->%s but ip_to_domain missing or mismatched", domain, addr)
			}
			return nil
		})
	})
}

func (s *FakeIPStore) GetOrAllocate(domain string) (netip.Addr, bool, error) {
	canonical := dns.CanonicalName(domain)

	s.mu.RLock()
	if addr, ok := s.byDomain[canonical]; ok {
		s.mu.RUnlock()
		return addr, false, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	if addr, ok := s.byDomain[canonical]; ok {
		return addr, false, nil
	}

	if s.next > s.last {
		return netip.Addr{}, false, ErrFakeIPPoolExhausted
	}

	nextVal := s.next
	addr := uint32ToAddr(nextVal)
	ipBytes := addrTo4Bytes(addr)

	err := s.db.Update(func(tx *bolt.Tx) error {
		d2p := tx.Bucket(bucketDomainToIP)
		p2d := tx.Bucket(bucketIPToDomain)
		meta := tx.Bucket(bucketMetadata)

		if err := d2p.Put([]byte(canonical), ipBytes); err != nil {
			return fmt.Errorf("write domain_to_ip: %w", err)
		}
		if err := p2d.Put(ipBytes, []byte(canonical)); err != nil {
			return fmt.Errorf("write ip_to_domain: %w", err)
		}

		if err := meta.Put(keyNext, uint32ToBytes(nextVal+1)); err != nil {
			return fmt.Errorf("write next cursor: %w", err)
		}
		return nil
	})
	if err != nil {
		return netip.Addr{}, false, err
	}

	s.next = nextVal + 1
	s.byDomain[canonical] = addr
	s.byIP[addr] = canonical
	s.updateStats()

	return addr, true, nil
}

func (s *FakeIPStore) LookupDomain(ip netip.Addr) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	domain, ok := s.byIP[ip]
	return domain, ok
}

func (s *FakeIPStore) Range(fn func(domain string, ip netip.Addr) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDomainToIP)
		return b.ForEach(func(k, v []byte) error {
			if len(v) != 4 {
				return fmt.Errorf("corrupt entry: expected 4 bytes for %s", string(k))
			}
			return fn(string(k), uint32ToAddr(binary.BigEndian.Uint32(v)))
		})
	})
}

func (s *FakeIPStore) Stats() FakeIPStats {
	ptr := s.stats.Load()
	if ptr == nil {
		return FakeIPStats{Prefix: s.prefix, StorePath: s.path}
	}
	return *ptr
}

func (s *FakeIPStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *FakeIPStore) updateStats() {
	allocated := uint64(len(s.byDomain))
	capacity := uint64(s.last - s.first + 1)
	var remaining uint64
	if s.next <= s.last {
		remaining = uint64(s.last - s.next + 1)
	}

	var nextAddr netip.Addr
	if s.next <= s.last {
		nextAddr = uint32ToAddr(s.next)
	}

	stats := &FakeIPStats{
		Prefix:       s.prefix,
		Capacity:     capacity,
		Allocated:    allocated,
		Remaining:    remaining,
		Next:         nextAddr,
		StorePath:    s.path,
		RecoveredDBs: s.recoveredDBs,
	}
	s.stats.Store(stats)
}

func addrToUint(addr netip.Addr) uint32 {
	b := addr.As4()
	return binary.BigEndian.Uint32(b[:])
}

func uint32ToAddr(n uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], n)
	return netip.AddrFrom4(b)
}

func uint32ToBytes(n uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return b
}

func addrTo4Bytes(addr netip.Addr) []byte {
	b := addr.As4()
	return b[:]
}

func broadcastAddr(prefix netip.Prefix) netip.Addr {
	addr := prefix.Addr()
	bits := prefix.Bits()
	n := addrToUint(addr)
	mask := uint32(0xFFFFFFFF) << (32 - bits)
	bcast := n | ^mask
	return uint32ToAddr(bcast)
}
