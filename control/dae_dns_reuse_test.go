/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"testing"

	"github.com/daeuniverse/dae/common/assets"
	"github.com/daeuniverse/dae/component/daedns"
	"github.com/daeuniverse/dae/config"
	"github.com/sirupsen/logrus"
)

func TestBuildDaeDNSRouterForControlPlane_ReusesPrebuiltRouter(t *testing.T) {
	prebuilt := &daedns.Router{}
	router, elapsed, reused, err := buildDaeDNSRouterForControlPlane(
		logrus.New(),
		&config.Global{},
		&config.Dns{},
		assets.NewLocationFinder(nil),
		controlPlaneBuildOptions{prebuiltDaeDNS: prebuilt},
	)
	if err != nil {
		t.Fatalf("buildDaeDNSRouterForControlPlane() error = %v", err)
	}
	if router != prebuilt {
		t.Fatalf("router = %p, want prebuilt %p", router, prebuilt)
	}
	if elapsed != 0 {
		t.Fatalf("elapsed = %v, want 0 for reused router", elapsed)
	}
	if !reused {
		t.Fatal("reused = false, want true")
	}
}
