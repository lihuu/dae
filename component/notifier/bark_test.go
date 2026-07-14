/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package notifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daeuniverse/dae/component/outbound"
	"github.com/sirupsen/logrus"
)

func newSilentLog() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.PanicLevel)
	return l
}

func TestBarkNotifier_DirectURLOverridesEnv(t *testing.T) {
	log := newSilentLog()
	direct := "https://api.day.app/DIRECT_TOKEN/"
	env := "https://api.day.app/ENV_TOKEN/"

	// direct URL wins when both are configured.
	n := NewBarkNotifier(log, direct, env, "", "", "", "", "")
	if got := n.baseURL; got != direct {
		t.Fatalf("baseURL = %q, want direct %q", got, direct)
	}
	if !n.Enabled() {
		t.Fatal("Enabled() = false, want true when direct URL present")
	}
}

func TestBarkNotifier_EnvUsedWhenDirectEmpty(t *testing.T) {
	log := newSilentLog()
	env := "https://api.day.app/ENV_TOKEN/"
	n := NewBarkNotifier(log, "", env, "", "", "", "", "")
	if got := n.baseURL; got != env {
		t.Fatalf("baseURL = %q, want env %q", got, env)
	}
	if !n.Enabled() {
		t.Fatal("Enabled() = false, want true when env URL present")
	}
}

func TestBarkNotifier_DisabledWhenNeitherURL(t *testing.T) {
	log := newSilentLog()
	n := NewBarkNotifier(log, "", "", "", "", "", "", "")
	if n.Enabled() {
		t.Fatal("Enabled() = true, want false when no URL configured")
	}
}

func TestBarkNotifier_SendDoesNotBlock(t *testing.T) {
	log := newSilentLog()
	// A server that hangs forever; Send must still return within the
	// per-request timeout (we use a short client timeout in the notifier).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second)
	}))
	defer srv.Close()

	n := NewBarkNotifier(log, srv.URL, "", "", "", "", "", "")
	done := make(chan struct{})
	go func() {
		n.Send(context.Background(), outbound.FailoverEvent{
			Type:         outbound.FailoverEventSwitch,
			Group:        "g",
			Primary:      "p",
			Fallback:     "f",
			From:         "primary",
			To:           "fallback",
			Trigger:      "tcp_unavailable",
			TransitionAt: time.Now(),
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Send blocked longer than the client timeout")
	}
}

func TestBarkNotifier_RenderSwitchTokens(t *testing.T) {
	log := newSilentLog()
	n := NewBarkNotifier(log, "https://api.day.app/x", "", "", "", "", "", "")
	ts := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	ev := outbound.FailoverEvent{
		Type:         outbound.FailoverEventSwitch,
		Group:        "proxy_failover",
		Primary:      "node-primary",
		Fallback:     "node-fallback",
		From:         "primary",
		To:           "fallback",
		Trigger:      "tcp_unavailable",
		TransitionAt: ts,
	}
	title, body := n.render(ev)
	if title != defaultSwitchTitle {
		t.Fatalf("switch title = %q, want default %q", title, defaultSwitchTitle)
	}
	wantBody := "proxy_failover: node-primary -> node-fallback; trigger=tcp_unavailable; at=" + ts.Format(time.RFC3339)
	if body != wantBody {
		t.Fatalf("switch body = %q, want %q", body, wantBody)
	}
}

func TestBarkNotifier_RenderFailbackTokens(t *testing.T) {
	log := newSilentLog()
	n := NewBarkNotifier(log, "https://api.day.app/x", "", "", "", "", "", "")
	ts := time.Date(2026, 7, 6, 12, 5, 0, 0, time.UTC)
	ev := outbound.FailoverEvent{
		Type:         outbound.FailoverEventFailbackComplete,
		Group:        "proxy_failover",
		Primary:      "node-primary",
		Fallback:     "node-fallback",
		From:         "fallback",
		To:           "primary",
		Successes:    3,
		StableFor:    30 * time.Second,
		TransitionAt: ts,
	}
	_, body := n.render(ev)
	wantBody := "proxy_failover: node-fallback -> node-primary; stable_for=30s; at=" + ts.Format(time.RFC3339)
	if body != wantBody {
		t.Fatalf("failback body = %q, want %q", body, wantBody)
	}
}

func TestBarkNotifier_UnknownTokenLeftLiteral(t *testing.T) {
	log := newSilentLog()
	n := NewBarkNotifier(log, "https://api.day.app/x", "",
		"Title {unknown_token}", "{group} body", "", "", "")
	ev := outbound.FailoverEvent{Type: outbound.FailoverEventSwitch, Group: "g"}
	title, body := n.render(ev)
	if title != "Title {unknown_token}" {
		t.Fatalf("title = %q, want unknown token left literal", title)
	}
	if body != "g body" {
		t.Fatalf("body = %q, want %q", body, "g body")
	}
}

func TestBarkNotifier_MissingFieldsRenderEmpty(t *testing.T) {
	log := newSilentLog()
	// Custom body references every token; event leaves most empty.
	n := NewBarkNotifier(log, "https://api.day.app/x", "",
		"", "{group}|{primary}|{fallback}|{from}|{to}|{trigger}|{transition_at}|{stable_for}|{successes}", "", "", "")
	ev := outbound.FailoverEvent{Type: outbound.FailoverEventSwitch}
	_, body := n.render(ev)
	want := "||||||||"
	if body != want {
		t.Fatalf("body = %q, want %q (all empty)", body, want)
	}
}

func TestBarkNotifier_NoSecretInLogs(t *testing.T) {
	// Capture log output.
	var buf strings.Builder
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)
	log.SetOutput(&buf)

	const secret = "SECRET_TOKEN_12345"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	// Embed the secret in the URL path (typical Bark token location).
	secretURL := srv.URL + "/" + secret + "/"

	n := NewBarkNotifier(log, secretURL, "", "", "", "", "", "")
	n.Send(context.Background(), outbound.FailoverEvent{
		Type:         outbound.FailoverEventSwitch,
		Group:        "g",
		Primary:      "p",
		Fallback:     "f",
		TransitionAt: time.Now(),
	})

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("log output leaked secret token:\n%s", out)
	}
	if !strings.Contains(out, "provider=bark") && !strings.Contains(out, "\"provider\":\"bark\"") {
		t.Fatalf("expected a redacted debug log entry, got:\n%s", out)
	}
}

// TestBarkNotifier_NoSecretInLogs_OnRequestError locks the redaction property
// on the request-error path. When client.Do errors, Go's HTTP client embeds
// the full URL in the error message (e.g. `Get "http://host/TOKEN/": ...`).
// errClass must strip the URL/token so it never reaches the logs.
func TestBarkNotifier_NoSecretInLogs_OnRequestError(t *testing.T) {
	var buf strings.Builder
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)
	log.SetOutput(&buf)

	const secret = "SECRET_TOKEN_REQERR_98765"
	// Port 1 refuses connections, producing a client.Do request_error without
	// needing an httptest server. The token is embedded in the URL path.
	unreachableURL := "http://127.0.0.1:1/" + secret + "/"

	n := NewBarkNotifier(log, unreachableURL, "", "", "", "", "", "")
	n.Send(context.Background(), outbound.FailoverEvent{
		Type:         outbound.FailoverEventSwitch,
		Group:        "g",
		Primary:      "p",
		Fallback:     "f",
		TransitionAt: time.Now(),
	})

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("log output leaked secret token on request-error path:\n%s", out)
	}
	// Confirm the request_error debug path was actually exercised.
	if !strings.Contains(out, "request_error") {
		t.Fatalf("expected a request_error debug log, got:\n%s", out)
	}
}

func TestBarkNotifier_SendHitsServer(t *testing.T) {
	log := newSilentLog()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := NewBarkNotifier(log, srv.URL, "", "", "", "", "", "")
	n.Send(context.Background(), outbound.FailoverEvent{
		Type:         outbound.FailoverEventSwitch,
		Group:        "g",
		Primary:      "p",
		Fallback:     "f",
		TransitionAt: time.Now(),
	})
	// Give the synchronous Send a moment to complete (it is synchronous).
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1", got)
	}
}

// TestBarkNotifier_ProxyTransportBuilt asserts that when a proxyURL is
// configured, the notifier's HTTP client gets a custom Transport (SOCKS5).
func TestBarkNotifier_ProxyTransportBuilt(t *testing.T) {
	log := newSilentLog()
	n := NewBarkNotifier(log, "https://api.day.app/x", "", "", "", "", "", "socks5://127.0.0.1:10808")
	if n.client.Transport == nil {
		t.Fatal("client.Transport = nil, want non-nil when proxyURL configured")
	}
}

// TestBarkNotifier_NoProxyWhenEmpty asserts that when proxyURL is empty,
// the notifier uses a plain http.Client with no custom Transport.
func TestBarkNotifier_NoProxyWhenEmpty(t *testing.T) {
	log := newSilentLog()
	n := NewBarkNotifier(log, "https://api.day.app/x", "", "", "", "", "", "")
	if n.client.Transport != nil {
		t.Fatal("client.Transport != nil, want nil when no proxy configured")
	}
}

// TestBarkNotifier_InvalidProxyFallsBackToDirect asserts that an invalid
// proxyURL does not panic and falls back to a direct client (Transport nil).
func TestBarkNotifier_InvalidProxyFallsBackToDirect(t *testing.T) {
	log := newSilentLog()
	// An unparseable URL with invalid characters.
	n := NewBarkNotifier(log, "https://api.day.app/x", "", "", "", "", "", "://invalid")
	if n.Enabled() {
		// The notifier should still be enabled (URL is valid); only the proxy failed.
		if n.client.Transport != nil {
			t.Fatal("client.Transport != nil, want nil when proxy init failed (fallback to direct)")
		}
	}
}
