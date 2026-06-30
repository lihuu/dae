// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>

package consts

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// generatedHeaderPath returns the path to control/kern/ebpf_sync_defs.h relative
// to this source file (common/consts/ebpf_sync_test.go).
func generatedHeaderPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// common/consts/ebpf_sync_test.go -> repo root
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	return filepath.Join(root, "control", "kern", "ebpf_sync_defs.h")
}

func TestOutboundRejectConstant(t *testing.T) {
	if OutboundReject != 0x2 {
		t.Fatalf("OutboundReject = 0x%X, want 0x2", OutboundReject)
	}
	if OutboundReject != OutboundBlock+1 {
		t.Fatalf("OutboundReject = 0x%X, want OutboundBlock+1 = 0x%X",
			OutboundReject, OutboundBlock+1)
	}
}

func TestOutboundUserDefinedMinExcludesReject(t *testing.T) {
	if OutboundUserDefinedMin <= OutboundReject {
		t.Fatalf("OutboundUserDefinedMin = 0x%X must be > OutboundReject = 0x%X",
			OutboundUserDefinedMin, OutboundReject)
	}
	if OutboundUserDefinedMin != OutboundReject+1 {
		t.Fatalf("OutboundUserDefinedMin = 0x%X, want OutboundReject+1 = 0x%X",
			OutboundUserDefinedMin, OutboundReject+1)
	}
}

// TestGoAndCOutboundConstantsAgree parses the generated C header and asserts the
// Go and C values match for REJECT and the user-defined bounds.
func TestGoAndCOutboundConstantsAgree(t *testing.T) {
	header, err := os.ReadFile(generatedHeaderPath(t))
	if err != nil {
		t.Fatalf("read generated header: %v", err)
	}
	lines := strings.Split(string(header), "\n")
	defines := map[string]string{}
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "#define ") {
			continue
		}
		fields := strings.Fields(ln)
		if len(fields) < 3 {
			continue
		}
		defines[fields[1]] = fields[2]
	}
	cases := []struct {
		cName string
		goVal uint8
	}{
		{"OUTBOUND_REJECT", uint8(OutboundReject)},
		{"OUTBOUND_BLOCK", uint8(OutboundBlock)},
		{"OUTBOUND_DIRECT", uint8(OutboundDirect)},
		{"OUTBOUND_USER_DEFINED_MIN", uint8(OutboundUserDefinedMin)},
	}
	for _, tc := range cases {
		got, ok := defines[tc.cName]
		if !ok {
			t.Errorf("C header missing #define %s", tc.cName)
			continue
		}
		if parseHex(t, got) != uint32(tc.goVal) {
			t.Errorf("%s: C=%s Go=0x%X disagree", tc.cName, got, tc.goVal)
		}
	}
	// OutboundUserDefinedMax is untyped (= 0xFB), so it is asserted separately
	// rather than via the uint8-typed table above.
	if maxDef, ok := defines["OUTBOUND_USER_DEFINED_MAX"]; !ok {
		t.Error("C header missing #define OUTBOUND_USER_DEFINED_MAX")
	} else if parseHex(t, maxDef) != uint32(OutboundUserDefinedMax) {
		t.Errorf("OUTBOUND_USER_DEFINED_MAX: C=%s Go=0x%X disagree", maxDef, OutboundUserDefinedMax)
	}
}

func parseHex(t *testing.T, s string) uint32 {
	t.Helper()
	s = strings.TrimPrefix(s, "0x")
	var v uint32
	for _, c := range s {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint32(c-'A') + 10
		default:
			t.Fatalf("bad hex digit %q in %q", c, s)
		}
	}
	return v
}
