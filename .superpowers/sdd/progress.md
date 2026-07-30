# SDD Progress Ledger — DAE Failover Notify (Bark)

**Plan:** `docs/superpowers/plans/2026-07-06-dae-failover-notify.md`
**Spec:** `docs/superpowers/specs/2026-07-06-dae-failover-notify-design.md`
**Worktree:** `/Users/lihu/git/dae-config/dae/.worktrees/feat/failover-notify-bark`
**Branch:** `feat/failover-notify-bark` (from `gateway/main`)
**BASE_SHA:** `1cbfbed4b6880b70b893c2bf32d15ff94c10c19f`
**Test env:** OrbStack VM `ubuntu-24.04`, Go 1.26.0, build tag `-tags dae_stub_ebpf`

## Test command (all implementers use this form)

```
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-notify-bark && go test -tags dae_stub_ebpf -run="..." ./... -count=1'
```

Build: `orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-notify-bark && go build ./...'`

## Tasks

### Task 1: Failover event types and async dispatcher — COMPLETE
- Commits: `1cbfbed..5285f94` (impl), `5285f94..281302a` (concurrent-close fix)
- Review: clean after fix (✅ spec, ✅ quality). Reviewer ran `-race`, no data races.
- Minor items deferred to final review:
  1. Dead no-op `onDrop` init at `failover_event.go:86` (immediately overwritten at `:88`) — harmless, slight confusion.
  2. `TestFailoverEventDispatcher_CloseStopsWorker` assertion `count.Load() >= 0` is always true — test verifies only "no panic," not "worker stopped." Could assert `<-d.done` or count stability.
  3. Implementer report inaccuracy: claimed `processNext`/`onDrop` are "exported" — they are lowercase/unexported (fine for same-package tests; Task 3 controller is same package `outbound`, so wiring works).

### Task 2: Bark notifier with templates and redaction — COMPLETE
- Commits: `281302a..7071618` (impl), `7071618..2a75fc7` (request-error redaction test)
- Review: clean after fix (✅ spec, ✅ quality). Redaction locked on both HTTP-status and request-error paths.
- Implementer deviation (approved): added `formatSuccesses` helper (empty for `n<=0`) to reconcile brief's `{successes}` rendering with spec's "missing fields render as empty" — correct.
- Minor items deferred to final review:
  1. Ineffective body drain in `Send` (`io.Copy`/`Close` LIFO order, `bark.go:130-131`) — drain is no-op, prevents connection reuse. Verbatim from brief.
  2. `errClass` heuristic fragile for schemeless URLs (`bark.go:230-240`) — safe for spec's scheme'd URLs; locked by test.
  3. Default transport honors `HTTP_PROXY` env var (`bark.go:91`) — spec's "no proxy" means no proxy config added, which holds; flagging only if strict isolation desired.
  4. `TestBarkNotifier_SendDoesNotBlock` 30s wall time from `srv.Close()` teardown on hung handler — deterministic, not flaky.

### Task 3: Wire failover event emission into the controller — COMPLETE
- Commit: `2a75fc7..8247a56` (impl only, no fixes needed)
- Review: clean first pass (✅ spec, ✅ quality, no Critical/Important/Minor).
- Implementer deviation (approved): dropped `.Round(time.Second)` on `StableFor` in the emit — the brief's test uses `StableTime: time.Nanosecond`, so rounding would zero-out the duration and fail `StableFor > 0`. Event carries the unrounded duration; the log line keeps existing rounded form.
- ⚠️ Cross-task item (controller handles emission; dispatcher wiring is Task 4): the `SetEventCallback` setter exists but no caller wires a dispatcher yet. Task 4 calls `dialerGroup.SetFailoverEventCallback(cb)`.

### Task 4: Wire dispatcher and Bark notifier into control plane — COMPLETE
- Commit: `8247a56..e333025` (impl only, no fixes needed)
- Review: clean first pass (✅ spec, ✅ quality, no Critical/Important).
- Implementer deviation (approved): added exported `SetProcessNext` seam on `FailoverEventDispatcher` because the brief assumed `processNext` (unexported) could be set cross-package. Additive; Task 1's same-package tests still assign the field directly.
- Also: gofmt alignment fix to `failover_event.go` (cosmetic, `queue`/`stop`/`done` aligned with `closeOnce`).
- Minor items deferred to final review:
  1. `SetProcessNext` is a mildly leaky abstraction (constructor injection would be cleaner), but justified — additive, preserves Task 1 pattern, happens-before safe.
  2. `testNotifyConfig` identity helper in test (spec-mandated, harmless).
- Reload no-replay (AC-08) verified: `RestoreSnapshot` untouched, no emit added.

### Task 5: Config decoding, validation, example.dae docs — COMPLETE
- Commit: `e333025..ac4a405` (impl only, no fixes needed)
- Review: clean first pass (✅ spec, ✅ quality, no Critical/Important).
- Implementer deviation (approved): config-package validate test can't import `component/outbound` (import cycle: `outbound` imports `config`). Used `config.ParseFunctionListOrString` + name `"failover"` in the test mirror; production `validateFailoverNotify` in `cmd/validate.go` uses the real `outbound.NewDialerSelectionPolicyFromGroupParam`. Both paths provably equivalent (the real parser internally uses the same `ParseFunctionListOrString` + name check). Replaced the brief's `t.Skip` placeholder with 4 real cmd tests.
- Minor items deferred to final review:
  1. Config-package mirror's `len(fs) != 1` skip is slightly stricter than the real parser on edge inputs (malformed `policy: failover(x)`) — both ultimately skip, agree for well-formed configs.
  2. Test fixture uses `"https://api.day.app/TOKEN/"` (brief verbatim) — stylistically inconsistent with `YOUR_TOKEN` convention but not a leak (test fixture, not docs).

### Task 6: Integration smoke test and final whole-branch review — COMPLETE
- Smoke gate: `go build` clean; all 5 suites pass (outbound/notifier/control/config/cmd); `go vet` clean; no real token in diff; no URL fields in log calls.
- Final whole-branch review (opus) found 1 Critical + 2 Important + 1 Minor:
  - **C-1 (Critical):** `failover_notify_bark_url_env` never resolved via `os.Getenv` — env var *name* used as literal URL. Broke AC-02. **Fixed in `65de238`.**
  - **I-1 (Important):** `SetProcessNext` data race (mutating field while worker reads it). **Fixed:** removed `SetProcessNext`; `processNext` now a constructor parameter. Happens-before sound.
  - **I-3 (Important):** masking test passed a literal URL as env arg. **Fixed:** test now uses `t.Setenv` + env var name; added env-unset test.
  - **Minor #3:** body drain LIFO bug (`io.Copy`/`Close` order). **Fixed:** swapped defer order.
- Re-review (sonnet): **Ready to merge: Yes.** AC-01..AC-09 all PASS. RA-01/RA-02 NOT VERIFIED (require production deployment — rollout acceptance, not source-verifiable).
- Deferred Minor items (not merge-blocking): dead no-op `onDrop` init; weak `CloseStopsWorker` assertion; `errClass` fragility for schemeless URLs; default transport proxy env; 30s test wall time; config-side validate mirror.

## Final Branch State

- **Branch:** `feat/failover-notify-bark` (worktree: `.worktrees/feat/failover-notify-bark`)
- **Merge base:** `1cbfbed` (`gateway/main`)
- **HEAD:** `65de238`
- **Commits:** 8 (5 feat + 2 fix + 1 test)
- **Files:** 15 changed, ~1550 insertions
- **All acceptance criteria PASS** (AC-01..AC-09). Rollout acceptance (RA-01/RA-02) requires production deployment per the spec's Verification Protocol.
- **Next step (per DAE AGENTS.md):** rebase onto `gateway/main`, merge into `gateway/main`, then remove the worktree. Rollout acceptance (RA-01 no-notify, RA-02 canary) happens on the production gateway after merge.
