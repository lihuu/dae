/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package config

import (
	"strings"
	"testing"

	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

func decodeConfigForTest(t *testing.T, raw string) *Config {
	t.Helper()
	sections, err := config_parser.Parse(raw)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	conf, err := New(sections)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	return conf
}

func minimalConfigWithDNS(t *testing.T, dnsBody string) *Config {
	t.Helper()
	return decodeConfigForTest(t, `
global {}
routing {
  fallback: direct
}
dns {
  upstream {
    cn: "udp://223.5.5.5:53"
  }
  routing {
    request {
      fallback: asis
    }
    response {
      fallback: accept
    }
  }
  fakeip {
    `+dnsBody+`
  }
}
`)
}

func TestNewUsesExplicitSectionDecoders(t *testing.T) {
	sections, err := config_parser.Parse(`
global {
  log_level: info
  so_mark_from_dae: 1234
}

subscription {
  "https://example.com/sub"
}

node {
  "ss://example"
}

group {
  proxy {
    policy: random
    filter: name(keyword: hk)
  }
}

routing {
  pname(NetworkManager) -> direct
  fallback: proxy
}

dns {
  ipversion_prefer: 6
  upstream {
    google:"8.8.8.8:53"
  }
  routing {
    request {
      qname(geosite:geolocation-!cn) -> proxy
      fallback: direct
    }
    response {
      fallback: proxy
    }
  }
}
`)
	require.NoError(t, err)

	conf, err := New(sections)
	require.NoError(t, err)
	require.True(t, conf.Global.SoMarkFromDaeSet)
	require.Len(t, conf.Subscription, 1)
	require.Len(t, conf.Node, 1)
	require.Len(t, conf.Group, 1)
	require.Equal(t, "proxy", conf.Group[0].Name)
	require.Equal(t, 6, conf.Dns.IpVersionPrefer)
	require.NotNil(t, conf.Routing.Fallback)
	require.NotNil(t, conf.Dns.Routing.Request.Fallback)
	require.NotNil(t, conf.Dns.Routing.Response.Fallback)
}

func TestDecodeConfigSectionRejectsUnknownSection(t *testing.T) {
	conf := &Config{}
	err := decodeConfigSection(conf, "unknown", &config_parser.Section{Name: "unknown"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown section")
}

func TestDecodeDNSFakeIP(t *testing.T) {
	conf := decodeConfigForTest(t, `
global {}
routing {
  fallback: direct
}
dns {
  upstream {
    cn: "udp://223.5.5.5:53"
  }
  fakeip {
    enabled: true
    inet4_range: 198.18.0.0/15
    ttl: 90
    store: /tmp/dae-fakeip.db
    direct_upstream: cn
  }
  routing {
    request {
      fallback: asis
    }
    response {
      fallback: accept
    }
  }
}
`)

	got := conf.Dns.FakeIP
	if !got.Enabled || got.Inet4Range != "198.18.0.0/15" ||
		got.TTL != 90 || got.Store != "/tmp/dae-fakeip.db" ||
		got.DirectUpstream != "cn" {
		t.Fatalf("unexpected fakeip config: %+v", got)
	}
}

func TestDNSFakeIPDefaults(t *testing.T) {
	conf := minimalConfigWithDNS(t, `
enabled: true
direct_upstream: cn
`)
	if got := conf.Dns.FakeIP.Inet4Range; got != "198.18.0.0/15" {
		t.Fatalf("inet4_range = %q", got)
	}
	if got := conf.Dns.FakeIP.TTL; got != 60 {
		t.Fatalf("ttl = %d", got)
	}
	if got := conf.Dns.FakeIP.Store; got != "/var/lib/dae/fakeip.db" {
		t.Fatalf("store = %q", got)
	}
}

func TestDNSFakeIPValidation(t *testing.T) {
	tests := []struct {
		name    string
		fakeip  string
		wantErr string
	}{
		{"missing direct upstream", "enabled: true", "direct_upstream is required"},
		{"invalid prefix", "enabled: true\ninet4_range: \"2001:db8::/32\"\ndirect_upstream: cn", "must be an IPv4 prefix"},
		{"network or broadcast only", "enabled: true\ninet4_range: 198.18.0.0/31\ndirect_upstream: cn", "has no allocatable IPv4 addresses"},
		{"zero ttl", "enabled: true\nttl: 0\ndirect_upstream: cn", "ttl must be positive"},
		{"unknown upstream", "enabled: true\ndirect_upstream: missing", `upstream "missing" not found`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sections, err := config_parser.Parse(`
global {}
routing {
  fallback: direct
}
dns {
  upstream {
    cn: "udp://223.5.5.5:53"
  }
  routing {
    request {
      fallback: asis
    }
    response {
      fallback: accept
    }
  }
  fakeip {
    ` + tc.fakeip + `
  }
}
`)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			_, err = New(sections)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}
