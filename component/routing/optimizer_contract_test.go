/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package routing

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/daeuniverse/dae/pkg/geodata"
	"github.com/sirupsen/logrus"
)

func TestCloneParamsCopiesSliceButSharesParamObjects(t *testing.T) {
	p0 := &config_parser.Param{Key: "k0", Val: "v0"}
	params := []*config_parser.Param{p0, nil}
	cloned := cloneParams(params)

	if len(cloned) != len(params) {
		t.Fatalf("unexpected len: %d", len(cloned))
	}
	if cloned[0] != p0 {
		t.Fatalf("expected shared param pointer")
	}

	cloned[0] = nil
	if params[0] == nil {
		t.Fatalf("expected independent slice container")
	}
}

func TestPostDatReaderOptimizersDoNotMutateCachedParams(t *testing.T) {
	originKey := string(consts.RoutingDomainKey_Suffix)
	originVal := "example.com"
	cached := []*config_parser.Param{
		{Key: originKey, Val: originVal},
	}

	hit1 := cloneParams(cached)
	hit2 := cloneParams(cached)

	rules := []*config_parser.RoutingRule{
		{
			AndFunctions: []*config_parser.Function{
				{
					Name: consts.Function_Domain,
					Params: []*config_parser.Param{
						hit1[0],
						{Key: string(consts.RoutingDomainKey_Keyword), Val: "example"},
					},
				},
			},
			Outbound: config_parser.Function{Name: "out"},
		},
		{
			AndFunctions: []*config_parser.Function{
				{
					Name:   consts.Function_Domain,
					Params: []*config_parser.Param{hit2[0]},
				},
			},
			Outbound: config_parser.Function{Name: "out"},
		},
	}

	var err error
	rules, err = (&MergeAndSortRulesOptimizer{}).Optimize(rules)
	if err != nil {
		t.Fatalf("MergeAndSortRulesOptimizer failed: %v", err)
	}
	_, err = (&DeduplicateParamsOptimizer{}).Optimize(rules)
	if err != nil {
		t.Fatalf("DeduplicateParamsOptimizer failed: %v", err)
	}

	if cached[0].Key != originKey || cached[0].Val != originVal {
		t.Fatalf("cached param mutated: got %q:%q", cached[0].Key, cached[0].Val)
	}
}

func TestDatReaderOptimizerGlobalGeoSiteCacheAcrossInstances(t *testing.T) {
	resetGlobalDatReaderCacheForTest()

	filePath := filepath.Join(t.TempDir(), "geosite.dat")
	if err := os.WriteFile(filePath, []byte("fake geosite"), 0644); err != nil {
		t.Fatal(err)
	}

	oldLocationAsset := datReaderGetLocationAsset
	oldUnmarshalGeoSite := datReaderUnmarshalGeoSite
	t.Cleanup(func() {
		datReaderGetLocationAsset = oldLocationAsset
		datReaderUnmarshalGeoSite = oldUnmarshalGeoSite
		resetGlobalDatReaderCacheForTest()
	})

	loads := 0
	datReaderGetLocationAsset = func(_ *DatReaderOptimizer, _ string) (string, error) {
		return filePath, nil
	}
	datReaderUnmarshalGeoSite = func(_ *logrus.Logger, _ string, code string) (*geodata.GeoSite, error) {
		loads++
		if code != "cn" {
			t.Fatalf("unexpected code %q", code)
		}
		return &geodata.GeoSite{
			Domain: []*geodata.Domain{
				{Type: geodata.Domain_RootDomain, Value: "example.cn"},
			},
		}, nil
	}

	first := &DatReaderOptimizer{Logger: logrus.New()}
	firstParams, err := first.loadGeoSite("geosite", "cn")
	if err != nil {
		t.Fatal(err)
	}
	second := &DatReaderOptimizer{Logger: logrus.New()}
	secondParams, err := second.loadGeoSite("geosite", "cn")
	if err != nil {
		t.Fatal(err)
	}

	if loads != 1 {
		t.Fatalf("geosite loads = %d, want 1", loads)
	}
	if len(firstParams) != 1 || len(secondParams) != 1 || firstParams[0].Val != secondParams[0].Val {
		t.Fatalf("unexpected params: first=%v second=%v", firstParams, secondParams)
	}
	if len(firstParams) > 0 && len(secondParams) > 0 && &firstParams[0] == &secondParams[0] {
		t.Fatalf("expected independent slice containers")
	}
}

func TestDatReaderOptimizerGlobalGeoIpCacheAcrossInstances(t *testing.T) {
	resetGlobalDatReaderCacheForTest()

	filePath := filepath.Join(t.TempDir(), "geoip.dat")
	if err := os.WriteFile(filePath, []byte("fake geoip"), 0644); err != nil {
		t.Fatal(err)
	}

	oldLocationAsset := datReaderGetLocationAsset
	oldUnmarshalGeoIp := datReaderUnmarshalGeoIp
	t.Cleanup(func() {
		datReaderGetLocationAsset = oldLocationAsset
		datReaderUnmarshalGeoIp = oldUnmarshalGeoIp
		resetGlobalDatReaderCacheForTest()
	})

	loads := 0
	datReaderGetLocationAsset = func(_ *DatReaderOptimizer, _ string) (string, error) {
		return filePath, nil
	}
	datReaderUnmarshalGeoIp = func(_ *logrus.Logger, _ string, code string) (*geodata.GeoIP, error) {
		loads++
		if code != "cn" {
			t.Fatalf("unexpected code %q", code)
		}
		ip := net.ParseIP("203.0.113.0").To4()
		return &geodata.GeoIP{
			Cidr: []*geodata.CIDR{
				{Ip: ip, Prefix: 24},
			},
		}, nil
	}

	first := &DatReaderOptimizer{Logger: logrus.New()}
	firstParams, err := first.loadGeoIp("geoip", "cn")
	if err != nil {
		t.Fatal(err)
	}
	second := &DatReaderOptimizer{Logger: logrus.New()}
	secondParams, err := second.loadGeoIp("geoip", "cn")
	if err != nil {
		t.Fatal(err)
	}

	want := netip.MustParsePrefix("203.0.113.0/24").String()
	if loads != 1 {
		t.Fatalf("geoip loads = %d, want 1", loads)
	}
	if len(firstParams) != 1 || len(secondParams) != 1 || firstParams[0].Val != want || secondParams[0].Val != want {
		t.Fatalf("unexpected params: first=%v second=%v want=%s", firstParams, secondParams, want)
	}
}
