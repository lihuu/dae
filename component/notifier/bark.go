// Package notifier delivers failover transition notifications to external
// providers. The first provider is Bark.
package notifier

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/daeuniverse/dae/component/outbound"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

// Default Bark notification templates (verbatim from the design spec).
const (
	defaultSwitchTitle   = "DAE failover switched"
	defaultSwitchBody    = "{group}: {primary} -> {fallback}; trigger={trigger}; at={transition_at}"
	defaultFailbackTitle = "DAE failover recovered"
	defaultFailbackBody  = "{group}: {fallback} -> {primary}; stable_for={stable_for}; at={transition_at}"
)

// httpTimeout caps a single Bark request. The notifier does not retry.
const httpTimeout = 10 * time.Second

// BarkNotifier sends one HTTP request per accepted FailoverEvent to a Bark
// endpoint. It is safe for concurrent use. The Bark URL/token is treated as
// sensitive: it never appears in logs.
type BarkNotifier struct {
	log           *logrus.Logger
	baseURL       string
	switchTitle   string
	switchBody    string
	failbackTitle string
	failbackBody  string
	client        *http.Client
}

// NewBarkNotifier constructs a BarkNotifier. directURL has priority over
// envURL; if both are empty the notifier is disabled (Enabled() returns
// false). Empty template strings fall back to the defaults.
//
// proxyURL is an optional SOCKS5 proxy URL (e.g. "socks5://127.0.0.1:10808")
// that routes notification HTTP requests through a local proxy instead of
// being captured by DAE's own transparent proxy. When proxyURL is empty or
// the proxy cannot be initialized, the notifier falls back to a direct
// http.Client.
func NewBarkNotifier(log *logrus.Logger, directURL, envURL, switchTitle, switchBody, failbackTitle, failbackBody, proxyURL string) *BarkNotifier {
	base := directURL
	if base == "" {
		base = envURL
	}
	if switchTitle == "" {
		switchTitle = defaultSwitchTitle
	}
	if switchBody == "" {
		switchBody = defaultSwitchBody
	}
	if failbackTitle == "" {
		failbackTitle = defaultFailbackTitle
	}
	if failbackBody == "" {
		failbackBody = defaultFailbackBody
	}
	client := &http.Client{Timeout: httpTimeout}
	if proxyURL != "" {
		if transport, err := newSOCKS5Transport(proxyURL); err == nil {
			client.Transport = transport
		} else if log != nil && log.IsLevelEnabled(logrus.DebugLevel) {
			log.WithFields(logrus.Fields{
				"provider": "bark",
				"reason":   "proxy_init_error",
			}).Debug("failover notify using direct (proxy unavailable)")
		}
	}
	return &BarkNotifier{
		log:           log,
		baseURL:       base,
		switchTitle:   switchTitle,
		switchBody:    switchBody,
		failbackTitle: failbackTitle,
		failbackBody:  failbackBody,
		client:        client,
	}
}

// Enabled reports whether the notifier has a usable base URL.
func (n *BarkNotifier) Enabled() bool {
	return n.baseURL != ""
}

// Send delivers one event. It is non-blocking up to httpTimeout: send errors,
// timeouts, and malformed responses are silent at info level and may appear
// only as redacted debug logs. A disabled notifier returns immediately.
func (n *BarkNotifier) Send(ctx context.Context, event outbound.FailoverEvent) {
	if !n.Enabled() {
		return
	}
	title, body := n.render(event)
	if title == "" || body == "" {
		n.debug(event, "empty_render", "")
		return
	}
	requestURL, err := n.buildURL(title, body)
	if err != nil {
		n.debug(event, "url_build_error", errClass(err))
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		n.debug(event, "request_build_error", errClass(err))
		return
	}
	resp, err := n.client.Do(req)
	if err != nil {
		n.debug(event, "request_error", errClass(err))
		return
	}
	defer resp.Body.Close()
	defer io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		n.debug(event, "http_status_class", httpStatusClass(resp.StatusCode))
	}
}

// render returns the title and body for an event by substituting supported
// tokens. Unknown tokens are left literal; missing fields render as empty.
func (n *BarkNotifier) render(event outbound.FailoverEvent) (string, string) {
	var title, body string
	switch event.Type {
	case outbound.FailoverEventSwitch:
		title = n.switchTitle
		body = n.switchBody
	case outbound.FailoverEventFailbackComplete:
		title = n.failbackTitle
		body = n.failbackBody
	default:
		return "", ""
	}
	return applyTemplate(title, event), applyTemplate(body, event)
}

// buildURL joins the Bark base URL with URL-escaped title/body path segments.
func (n *BarkNotifier) buildURL(title, body string) (string, error) {
	base := strings.TrimRight(n.baseURL, "/")
	if base == "" {
		return "", fmt.Errorf("empty base url")
	}
	// Bark expects: {base}/{title}/{body} with path-escaped segments.
	return base + "/" + url.PathEscape(title) + "/" + url.PathEscape(body), nil
}

// debug emits a redacted debug log. It must never include the URL, token, or
// request path.
func (n *BarkNotifier) debug(event outbound.FailoverEvent, reason, errClass string) {
	if n.log == nil || !n.log.IsLevelEnabled(logrus.DebugLevel) {
		return
	}
	n.log.WithFields(logrus.Fields{
		"provider":    "bark",
		"group":       event.Group,
		"event_type":  string(event.Type),
		"reason":      reason,
		"error_class": errClass,
	}).Debug("failover notify send")
}

// applyTemplate replaces supported tokens with event values. Unknown tokens
// are left unchanged.
func applyTemplate(tmpl string, event outbound.FailoverEvent) string {
	replacements := []struct {
		token string
		value string
	}{
		{"{group}", event.Group},
		{"{primary}", event.Primary},
		{"{fallback}", event.Fallback},
		{"{from}", event.From},
		{"{to}", event.To},
		{"{trigger}", event.Trigger},
		{"{transition_at}", formatTransitionAt(event.TransitionAt)},
		{"{stable_for}", formatStableFor(event.StableFor)},
		{"{successes}", formatSuccesses(event.Successes)},
	}
	out := tmpl
	for _, r := range replacements {
		out = strings.ReplaceAll(out, r.token, r.value)
	}
	return out
}

func formatTransitionAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func formatStableFor(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.Round(time.Second).String()
}

// formatSuccesses renders the success count. A zero count is treated as a
// missing field and renders as empty (consistent with other optional fields).
func formatSuccesses(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}

func httpStatusClass(code int) string {
	return fmt.Sprintf("%dxx", code/100)
}

func errClass(err error) string {
	if err == nil {
		return ""
	}
	// Keep only the top-level error message type; never include URLs.
	msg := err.Error()
	if i := strings.Index(msg, ":"); i > 0 {
		return msg[:i]
	}
	return msg
}

// newSOCKS5Transport builds an http.RoundTripper that dials through a SOCKS5
// proxy. The proxy URL must be a valid URL with a resolvable host (e.g.
// "socks5://127.0.0.1:10808"). No authentication is configured; local Xray
// SOCKS5 does not require it.
func newSOCKS5Transport(proxyURL string) (http.RoundTripper, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, fmt.Errorf("empty proxy host")
	}
	dialer, err := proxy.SOCKS5("tcp", u.Host, nil, proxy.Direct)
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// proxy.SOCKS5 returns a dialer that also implements ContextDialer;
			// prefer DialContext to respect cancellation/timeouts.
			if cd, ok := dialer.(proxy.ContextDialer); ok {
				return cd.DialContext(ctx, network, addr)
			}
			return dialer.Dial(network, addr)
		},
	}, nil
}
