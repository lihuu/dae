# FakeIP Observability Implementation Tracker

**Spec:** `dae/docs/superpowers/specs/2026-06-17-fakeip-observability-design.md`
**Goal:** Add structured FakeIP observability events to DAE and an operations parser tool.

## Phase 1: DAE Event Helpers

- [ ] 1.1 Create `control/fakeip_observability.go` with event helper functions
- [ ] 1.2 Write `control/fakeip_observability_test.go` — assert fields on every event type

## Phase 2: Insert Events at Code Points

- [ ] 2.1 `fakeip_dns_answer` in `handleFakeIPQuery` (dns_control.go)
- [ ] 2.2 `fakeip_flow` in TCP `handleConn` (tcp.go)
- [ ] 2.3 `fakeip_flow` in UDP flow handler (udp.go)
- [ ] 2.4 `fakeip_unknown` in TCP + UDP unknown mapping paths
- [ ] 2.5 `fakeip_direct_resolve` in TCP + UDP direct resolution paths
- [ ] 2.6 `fakeip_dial_error` in dial failure paths
- [ ] 2.7 `fakeip_reload` in reload replay paths

## Phase 3: DAE Tests

- [ ] 3.1 Update existing FakeIP tests to assert event emission via log hook
- [ ] 3.2 Run full DAE test suite on OrbStack VM

## Phase 4: Operations Parser

- [ ] 4.1 Implement `dae-fakeip-log` Python script in `docs/network/dae-config/tools/`
- [ ] 4.2 Write `test_dae_fakeip_log.py` unittests
- [ ] 4.3 Verify parser against synthetic log fixtures

## Phase 5: Production Acceptance

- [ ] 5.1 Deploy DAE binary with observability events to vm-ubuntu-agent
- [ ] 5.2 Deploy parser to vm-ubuntu-agent
- [ ] 5.3 Run acceptance workflow from spec section "Production acceptance"

## Verification Log

### 2026-06-17T11:28 — DAE event helpers + integration
- **Task**: Phase 1-3 (event helpers, insertion, tests)
- **Command**: `go test -v -run "TestFakeIP|TestLogFakeIP" ./control/`
- **Result**: 37/37 PASS — all new observability tests and existing FakeIP tests pass

### 2026-06-17T11:28 — Full control package tests
- **Task**: Regression check
- **Command**: `go test -count=1 ./control/`
- **Result**: PASS (19.6s) — no regressions

### 2026-06-17T11:28 — Parser tests
- **Task**: Phase 4 (Python parser tests)
- **Command**: `python3 -m unittest test_dae_fakeip_log -v`
- **Result**: 21/21 PASS

### 2026-06-17 (post-acceptance review) — Reload-failure wiring + unknown rate-limit fix
- **Task**: Acceptance gaps:
  1. `logFakeIPReloadFailed` was defined but never called from production code.
  2. `logFakeIPUnknown` ran outside the `allowUnknownFakeIPLog` rate-limit gate.
- **Changes**:
  - `dns_control.go`: `replayFakeIPMappings` now returns `(count, failed, rangeErr)`.
  - `control_plane.go`: replay caller emits `fakeip_reload result=failed` on any
    range error or per-entry publish failure; success path unchanged.
  - `tcp.go` / `udp.go`: `logFakeIPUnknown` and the human warn now share one
    rate-limit token via `allowUnknownFakeIPLog` (spec: "warn, rate-limited").
  - `control_plane_reload_test.go`: added `TestReplayFakeIPMappingsCountsFailures`.
- **Command**: `go test -tags dae_stub_ebpf -count=1 ./control/` on Ubuntu 24.04
- **Result**: PASS (19.5s) — full control package, no regressions.
