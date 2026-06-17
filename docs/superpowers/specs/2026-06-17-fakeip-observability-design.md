# FakeIP Observability Design

## Goal

Add structured FakeIP observability so operators and AI assistants can explain
FakeIP behavior from production logs without reading noisy generic debug output
or modifying the running gateway during an incident.

The first version must:

- Keep DAE as the only producer of authoritative runtime events.
- Write FakeIP events into the existing DAE log stream using stable logrus
  fields.
- Keep analysis outside the DAE binary in an operations tool.
- Make the event schema stable enough for scripts and AI-assisted diagnosis.
- Explain whether a domain received FakeIP, how traffic was routed, what dial
  target was used, and where failures happened.
- Quantify the `FakeIP + direct` intersection so future `auto` DNS routing can
  be designed from evidence instead of assumptions.

## Non-goals

The first version does not implement:

- DNS `auto` mode.
- A metrics server or management API.
- A separate FakeIP event log file.
- Log shipping, dashboards, or long-term storage.
- Packet capture or automatic WAN leakage detection.
- FakeIP address reclamation.
- Changes to routing behavior.

## Architecture

DAE emits structured events into the normal log file, currently
`/var/log/dae/dae.log`.

Each event uses ordinary logrus fields. The human-readable message is stable but
not parsed by tools:

```text
msg="fakeip_event" component=fakeip event=fakeip_flow event_version=1 ...
```

The operations tool reads DAE logs, filters records with `component=fakeip`,
parses fields, and produces summaries or JSON for AI analysis.

```text
DAE runtime
  -> logrus structured FakeIP fields
  -> /var/log/dae/dae.log
  -> operations parser
  -> human summary or machine JSON
```

This keeps DAE focused on recording facts. The parser can evolve quickly in the
operations workspace without changing or redeploying the gateway binary.

## Event Format

Every FakeIP event must include:

```text
component=fakeip
event=<event-name>
event_version=1
```

Field names are lowercase snake case. Values must be single-line and safe for
logrus text output. Missing optional fields are omitted rather than emitted as
empty strings, except where an empty value is materially different from absence.

Tools must parse fields, not `msg`.

### Common Fields

Common fields used when available:

```text
domain=<canonical-domain-with-trailing-dot>
fakeip=<synthetic-ipv4>
client=<client-ip>
client_port=<source-port>
network=tcp|udp|dns
port=<destination-port>
outbound=<direct|must_direct|proxy|proxy_failover|block|...>
policy=<fixed|failover|direct|...>
dial_target=<domain-or-ip:port>
result=ok|failed|rejected
error_class=<stable-error-class>
error=<short-error-message>
```

`domain` should use the canonical form already used by FakeIP storage. The
parser may normalize display names, but DAE should emit one canonical spelling.

### Error Classes

`error_class` should be stable and coarse-grained:

```text
fakeip_not_configured
fakeip_store_error
fakeip_cache_error
fakeip_unknown_mapping
direct_resolve_failed
proxy_dial_failed
udp_endpoint_failed
reload_replay_failed
```

Free-form `error` is allowed for operators, but tools must group by
`error_class`.

## Events

### `fakeip_dns_answer`

Emitted when DAE answers a DNS query with a FakeIP or a FakeIP NODATA response.

Required fields:

```text
event=fakeip_dns_answer
domain=www.wikipedia.org.
qtype=A|AAAA|HTTPS|...
result=ok|failed
```

For A answers:

```text
fakeip=198.18.0.102
ttl=60
allocated=true|false
```

For non-A queries routed to FakeIP:

```text
answer=nodata
```

Failures must include `error_class`.

Recommended level:

- `debug` for successful answers.
- `warn` for failed answers.

### `fakeip_flow`

Emitted once per accepted FakeIP TCP connection or UDP flow after business
routing has selected the outbound and DAE has recovered the domain.

Required fields:

```text
event=fakeip_flow
domain=www.wikipedia.org.
fakeip=198.18.0.102
client=192.168.2.7
network=tcp|udp
port=443
outbound=proxy_failover
result=ok|rejected
```

When known:

```text
policy=failover
dial_target=www.wikipedia.org:443
```

Recommended level:

- `info` during early production rollout.
- Later configurable or downgraded to `debug` for successful proxy flows.
- Always `info` or higher for `outbound=direct` and `outbound=must_direct`.

### `fakeip_direct_resolve`

Emitted when a FakeIP flow's final business outbound is `direct` or
`must_direct` and DAE resolves the recovered domain through
`dns.fakeip.direct_upstream`.

Required fields:

```text
event=fakeip_direct_resolve
domain=example.com.
fakeip=198.18.0.130
upstream=cn
network=tcp|udp
port=443
result=ok|failed
```

On success:

```text
resolved_ip=93.184.216.34
```

On failure:

```text
error_class=direct_resolve_failed
error=<short-error-message>
```

Recommended level:

- `info` on success.
- `warn` on failure.

### `fakeip_unknown`

Emitted when traffic targets the configured FakeIP CIDR but no mapping exists.

Required fields:

```text
event=fakeip_unknown
fakeip=198.18.1.12
client=192.168.2.9
network=tcp|udp
port=443
result=rejected
error_class=fakeip_unknown_mapping
```

Recommended level: `warn`, rate-limited.

### `fakeip_dial_error`

Emitted when DAE has a known FakeIP mapping and selected outbound, but dialing
or relay setup fails.

Required fields:

```text
event=fakeip_dial_error
domain=www.github.com.
fakeip=198.18.0.120
client=192.168.2.7
network=tcp|udp
port=443
outbound=proxy_failover
dial_target=www.github.com:443
result=failed
error_class=proxy_dial_failed
error=<short-error-message>
```

Recommended level: `warn`.

### `fakeip_reload`

Emitted when reload reuses or replays FakeIP state.

Required fields:

```text
event=fakeip_reload
result=ok|failed
```

On success:

```text
replayed=103
store=/var/lib/dae/fakeip.db
cidr=198.18.0.0/15
```

On failure:

```text
error_class=reload_replay_failed
error=<short-error-message>
```

Recommended level:

- `info` on success.
- `warn` or `error` on failure depending on whether reload continues.

## Logging Volume

The first rollout is intentionally verbose for FakeIP flows because production
is still in whitelist mode.

Initial policy:

- Successful `fakeip_flow` events are `info`.
- Successful `fakeip_dns_answer` events are `debug`.
- Direct, unknown, and failure events are always visible at `info` or `warn`.

Future policy may add configuration:

```dae
fakeip {
    observability: true
    flow_log_level: info
    dns_answer_log_level: debug
}
```

This configuration is not required for the first implementation. If omitted,
FakeIP observability follows the fixed level policy above.

## Operations Tool

The parser lives outside the DAE binary. Candidate names:

```text
dae-fakeip-log
dae-logs fakeip
```

It should fit the existing operations style used by tools such as `dae-logs`
and `dae-config`.

Default input:

```text
/var/log/dae/dae.log
```

### Commands

Minimum command set:

```bash
dae-fakeip-log summary --since 1h
dae-fakeip-log domain github.com --since 1h
dae-fakeip-log errors --since 1h
dae-fakeip-log direct --since 24h
dae-fakeip-log json --since 1h
```

Useful options:

```text
--file /var/log/dae/dae.log
--since 1h|30m|2026-06-17T10:00:00+08:00
--domain github.com
--client 192.168.2.7
--event fakeip_flow
--outbound proxy_failover
--json
```

### Summary Output

Human summary should answer:

```text
FakeIP summary last 1h
dns_answers: 120
flows_total: 84
proxy_flows: 40
proxy_failover_flows: 39
direct_flows: 2
must_direct_flows: 0
udp_flows: 3
unknown_fakeip: 0
dial_errors: 5

Top failed domains:
github.com 4
youtube.com 1

FakeIP + direct:
example.com 2
```

JSON output should expose the same aggregates plus grouped raw events where
requested. AI workflows should prefer `--json` for exact analysis.

## Diagnosis Workflows

### GitHub or YouTube does not open

Run:

```bash
dae-fakeip-log domain github.com --since 30m
dae-fakeip-log errors --since 30m --domain github.com
```

The output must show which stage failed:

- No `fakeip_dns_answer`: DNS rule did not select FakeIP or client did not use
  DAE DNS.
- `fakeip_dns_answer` exists but no `fakeip_flow`: client did not connect to
  the synthetic address or traffic bypassed DAE.
- `fakeip_flow` has `outbound=direct`: domain should probably use real DNS or
  future `auto` needs to classify it as direct.
- `fakeip_dial_error`: proxy dialing or selected outbound failed.
- `fakeip_unknown`: stale client cache or missing mapping after database reset.

### Evaluate readiness for DNS `auto`

Run:

```bash
dae-fakeip-log summary --since 24h
dae-fakeip-log direct --since 24h
```

`auto` design should not proceed until direct-flow data is understood. If
`FakeIP + direct` is not rare, the DNS policy needs real-DNS exclusions before
broader FakeIP rollout.

## Testing

### DAE unit tests

Tests should verify:

- Each event helper emits `component=fakeip` and `event_version=1`.
- `fakeip_dns_answer` includes domain, qtype, TTL, allocation status, and
  FakeIP for A answers.
- `fakeip_flow` includes recovered domain, selected outbound, network, port,
  and dial target when available.
- Direct resolution success and failure emit `fakeip_direct_resolve`.
- Unknown mappings emit rate-limited `fakeip_unknown`.
- Dial failures emit `fakeip_dial_error`.
- Reload replay emits `fakeip_reload`.

Tests must assert fields, not formatted strings.

### Operations parser tests

Use sample log lines containing ordinary DAE noise and FakeIP events. Verify:

- Parser ignores non-FakeIP lines.
- Parser accepts logrus field order changes.
- Parser groups by domain suffix.
- Parser counts direct/proxy/UDP/error categories correctly.
- `--json` returns valid JSON.
- Time filtering handles DAE's current timestamp format.

### Production acceptance

On `vm-ubuntu-agent` with `wikipedia.org` FakeIP enabled:

1. Query `wikipedia.org A` and confirm a `fakeip_dns_answer` event.
2. Access `https://wikipedia.org` from a LAN client and confirm a
   `fakeip_flow` event with `outbound=proxy_failover`.
3. Reload DAE and confirm a `fakeip_reload` event.
4. Query a new `*.wikipedia.org` name and confirm the analyzer reports the new
   allocation.
5. Confirm `dae-fakeip-log summary --since 10m` reports zero unknown mappings
   and zero direct flows for the acceptance window.

## Rollout

1. Implement DAE event helpers and add focused tests.
2. Add event calls at DNS answer, TCP, UDP, direct-resolve, unknown-mapping,
   dial-error, and reload replay points.
3. Implement the external parser in the operations workspace.
4. Deploy to production with only `wikipedia.org` FakeIP enabled.
5. Verify analyzer output against live behavior.
6. Use the analyzer to test GitHub and YouTube whitelist expansion.
7. Revisit DNS `auto` only after direct-flow and failure data is available.

## Decisions

- Successful `fakeip_flow` events use `info` in the first implementation. The
  feature is still in whitelist rollout, and these events are the primary
  evidence for GitHub and YouTube diagnosis. If log volume becomes a problem
  after broader rollout, a later version may add configuration or downgrade
  successful proxy flows to `debug`.
- The first parser is an operations tool, not a DAE subcommand. It should be
  implemented as `dae-logs fakeip` if that fits the existing operations tooling
  structure; otherwise use a standalone `dae-fakeip-log` wrapper with the same
  command surface. In both cases, parser behavior and output schema are defined
  by this spec, not by the command name.
- The first DAE implementation does not add a new `observability` configuration
  block. Event emission follows the fixed log-level policy in this document.
  Configuration can be added later only if measured production volume requires
  it.
