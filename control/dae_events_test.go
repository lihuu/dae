// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>

package control

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestRenderDaeEventDistinguishesRejectAndBlock(t *testing.T) {
	mk := func(evtType uint32, sport, dport uint16) []byte {
		e := bpfDaeEvent{
			Type:     evtType,
			Outbound: 0x2,
			L4proto:  1, // TCP
			Sport:    sport,
			Dport:    dport,
		}
		// IPv4 client 192.168.2.9 -> dest 198.51.100.1, embedded in the
		// first u32 of the IPv6-mapped representation.
		e.Sip[0] = binary.BigEndian.Uint32([]byte{192, 168, 2, 9})
		e.Dip[0] = binary.BigEndian.Uint32([]byte{198, 51, 100, 1})
		var buf bytes.Buffer
		if err := binary.Write(&buf, binary.LittleEndian, &e); err != nil {
			t.Fatalf("binary.Write: %v", err)
		}
		return buf.Bytes()
	}
	rj := renderDaeEvent(mk(daeEventTypeRejected, 54321, 443))
	if !strings.Contains(rj, "reject") {
		t.Fatalf("reject render = %q, want substring \"reject\"", rj)
	}
	if strings.Contains(rj, "block") {
		t.Fatalf("reject render must not contain \"block\": %q", rj)
	}
	bl := renderDaeEvent(mk(daeEventTypeBlocked, 54321, 443))
	if !strings.Contains(bl, "block") {
		t.Fatalf("block render = %q, want substring \"block\"", bl)
	}
}
