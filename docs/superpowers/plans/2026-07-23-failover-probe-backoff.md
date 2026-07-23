# Configurable Failover Probe Backoff Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add selectable fixed or exponential failover recovery-probe pacing and reset the probe delay to the initial interval whenever recovery advances to another Primary candidate.

**Architecture:** Extend `config.Group` with one string field that is parsed into a compact outbound enum, then keep both modes inside the existing `FailoverController`. One pure helper computes the next failed-probe delay; reload compatibility compares effective recovery semantics so fixed mode ignores `recovery_probe_max` without weakening identity checks for other fields.

**Tech Stack:** Go 1.26+, DAE configuration parser, `component/outbound` failover controller, deterministic fake scheduler, Logrus structured-log tests, OrbStack Ubuntu 24.04 Linux tests, Git worktrees.

## Global Constraints

- `recovery_probe_backoff` accepts case-sensitive `exponential` and `fixed`; omission defaults to `exponential`.
- Fixed mode schedules every recovery and confirmation probe after `recovery_probe_initial`; it does not use `recovery_probe_max`.
- Exponential mode doubles the same-candidate delay and caps it at `recovery_probe_max` without duration overflow.
- Candidate advancement is controlled only by positive `primary_rotation_attempts`; `recovery_probe_max` never advances a candidate.
- Every candidate advancement, including wraparound and a single-candidate modulo advance, resets the next delay to `recovery_probe_initial` and does not probe immediately.
- `primary_rotation_attempts: 0` pins the current recovery target while the selected backoff calculation continues.
- Success confirmation remains on the shared controller path and uses `recovery_probe_initial`.
- Keep one `FailoverController`; do not add parallel probing, a strategy interface, a second probe loop, or mode-specific promotion/reload/notification paths.
- Keep the existing 10-second network-probe timeout, fixed Fallback traffic semantics, memory-only state, and restart-from-`primary[0]` behavior.
- Fixed mode accepts syntactically valid positive, zero, or negative `recovery_probe_max` durations and ignores them; malformed duration text still fails generic parsing.
- Effective reload identity includes mode, initial delay, successes, stable time, rotation attempts, ordered Primary names, and Fallback; maximum participates only in exponential mode.
- A source implementation or binary deployment must not silently add fixed mode to live production configuration; production opt-in is a separately authorized operation.
- Run Linux tests in OrbStack with `GOCACHE=/tmp/dae-go-cache`; preserve unrelated user changes and work only in `/Users/lihu/git/dae-config/dae/.worktrees/feat/failover-probe-backoff`.

---

### Task 1: Decode and Validate the Backoff Mode

**Files:**
- Create: `component/outbound/failover_backoff.go`
- Modify: `config/config.go:137-151`
- Modify: `config/failover_rotation_test.go`
- Modify: `control/group_dialer_resolver.go:17-44`
- Modify: `control/group_dialer_resolver_test.go`
- Modify: `component/outbound/failover_controller.go:87-96`
- Modify: `component/outbound/failover_roles.go:59-80`
- Modify: `component/outbound/failover_roles_test.go`

**Interfaces:**
- Produces: `type FailoverProbeBackoff uint8`.
- Produces: `FailoverProbeBackoffExponential` as the zero value and `FailoverProbeBackoffFixed` as the second enum value.
- Produces: `ParseFailoverProbeBackoff(raw string) (FailoverProbeBackoff, error)`.
- Produces: `FailoverRecoveryConfig.Backoff FailoverProbeBackoff`.
- Consumes: `config.Group.RecoveryProbeBackoff string`, defaulting to `"exponential"`.

- [ ] **Step 1: Write failing parser/default tests**

Extend `config/failover_rotation_test.go` with exact default and explicit-value assertions:

```go
func TestGroupRecoveryProbeBackoffDefaultsToExponential(t *testing.T) {
	g := decodeRotationGroup(t, "")
	require.Equal(t, "exponential", g.RecoveryProbeBackoff)
}

func TestGroupRecoveryProbeBackoffDecodesExplicitValue(t *testing.T) {
	g := decodeRotationGroup(t, "recovery_probe_backoff: fixed")
	require.Equal(t, "fixed", g.RecoveryProbeBackoff)
}
```

Add an error-returning decoder helper and prove a malformed duration remains a parser error even when fixed is selected:

```go
func TestGroupFixedBackoffStillRejectsMalformedMaxDuration(t *testing.T) {
	raw := `
global {}
routing { fallback: direct }
group {
  proxy_failover {
    primary: name(A)
    fallback: name(X)
    policy: failover
    recovery_probe_backoff: fixed
    recovery_probe_max: definitely-not-a-duration
  }
}`
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	_, err = New(sections)
	require.Error(t, err)
}
```

- [ ] **Step 2: Write failing enum and mode-aware validation tests**

In `component/outbound/failover_roles_test.go`, add table tests that call the production parser and `validateFailoverRecoveryConfig`:

```go
func TestParseFailoverProbeBackoff(t *testing.T) {
	tests := []struct {
		raw     string
		want    FailoverProbeBackoff
		wantErr string
	}{
		{raw: "exponential", want: FailoverProbeBackoffExponential},
		{raw: "fixed", want: FailoverProbeBackoffFixed},
		{raw: "", wantErr: `recovery_probe_backoff ""`},
		{raw: "Fixed", wantErr: `recovery_probe_backoff "Fixed"`},
		{raw: "linear", wantErr: `recovery_probe_backoff "linear"`},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := ParseFailoverProbeBackoff(tt.raw)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.ErrorContains(t, err, "exponential, fixed")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestValidateFailoverRecoveryConfigBackoffModes(t *testing.T) {
	base := FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax: 5 * time.Minute,
		Successes: 3,
		StableTime: 30 * time.Second,
		RotationAttempts: 5,
	}
	for _, max := range []time.Duration{5 * time.Minute, 0, -time.Second} {
		fixed := base
		fixed.Backoff = FailoverProbeBackoffFixed
		fixed.ProbeMax = max
		require.NoError(t, validateFailoverRecoveryConfig(fixed))
	}
	exponential := base
	exponential.ProbeMax = 0
	require.ErrorContains(t, validateFailoverRecoveryConfig(exponential), "recovery_probe_max must be positive")
	exponential.ProbeMax = time.Second
	require.ErrorContains(t, validateFailoverRecoveryConfig(exponential), "must not exceed")
}
```

Include fixed and exponential cases with `ProbeInitial` equal to zero and `-time.Second`; both must return `recovery_probe_initial must be positive`.

- [ ] **Step 3: Run the focused tests and verify RED**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./config ./component/outbound -run 'Test(GroupRecoveryProbeBackoff|GroupFixedBackoff|ParseFailoverProbeBackoff|ValidateFailoverRecoveryConfigBackoffModes)' -count=1
```

Expected: FAIL to compile because the group field, enum, parser, and `Backoff` recovery field do not exist.

- [ ] **Step 4: Add the config field and compact enum parser**

Add to `config.Group` immediately before the existing recovery durations:

```go
RecoveryProbeBackoff string `mapstructure:"recovery_probe_backoff" default:"exponential"`
```

Create `component/outbound/failover_backoff.go`:

```go
package outbound

import "fmt"

type FailoverProbeBackoff uint8

const (
	FailoverProbeBackoffExponential FailoverProbeBackoff = iota
	FailoverProbeBackoffFixed
)

func ParseFailoverProbeBackoff(raw string) (FailoverProbeBackoff, error) {
	switch raw {
	case "exponential":
		return FailoverProbeBackoffExponential, nil
	case "fixed":
		return FailoverProbeBackoffFixed, nil
	default:
		return 0, fmt.Errorf(
			"invalid recovery_probe_backoff %q: accepted values are exponential, fixed",
			raw,
		)
	}
}
```

Add the enum as the first field of `FailoverRecoveryConfig`. Keeping exponential as enum zero preserves direct constructors and existing test literals that omit the new field:

```go
type FailoverRecoveryConfig struct {
	Backoff         FailoverProbeBackoff
	ProbeInitial    time.Duration
	ProbeMax        time.Duration
	Successes       int
	StableTime      time.Duration
	RotationAttempts int
}
```

- [ ] **Step 5: Parse the group string before resolving failover roles**

In `resolveConfiguredGroupDialers`, parse and wrap the raw value before building the recovery struct:

```go
backoff, err := outbound.ParseFailoverProbeBackoff(group.RecoveryProbeBackoff)
if err != nil {
	return nil, nil, nil, fmt.Errorf("group %q: %w", group.Name, err)
}
recovery := outbound.FailoverRecoveryConfig{
	Backoff:         backoff,
	ProbeInitial:    group.RecoveryProbeInitial,
	ProbeMax:        group.RecoveryProbeMax,
	Successes:       group.RecoverySuccesses,
	StableTime:      group.RecoveryStableTime,
	RotationAttempts: group.PrimaryRotationAttempts,
}
```

Set `RecoveryProbeBackoff: "exponential"` inside the existing `withValidFailoverRecovery` test helper because manually constructed `config.Group` values do not pass through default decoding. Add production-path table cases for `fixed`, `exponential`, empty, misspelled, and unknown values in `control/group_dialer_resolver_test.go`.

- [ ] **Step 6: Make recovery validation mode-aware**

Keep initial, successes, stable time, and rotation-attempt validation common, but gate maximum checks by enum:

```go
switch recovery.Backoff {
case FailoverProbeBackoffFixed:
	// ProbeMax is syntactically decoded by config but semantically unused.
case FailoverProbeBackoffExponential:
	if recovery.ProbeMax <= 0 {
		return fmt.Errorf("recovery_probe_max must be positive")
	}
	if recovery.ProbeInitial > recovery.ProbeMax {
		return fmt.Errorf(
			"recovery_probe_initial (%v) must not exceed recovery_probe_max (%v)",
			recovery.ProbeInitial,
			recovery.ProbeMax,
		)
	}
default:
	return fmt.Errorf("invalid failover recovery backoff enum: %d", recovery.Backoff)
}
```

Do not move string parsing into the controller and do not give an invalid internal enum exponential behavior.

- [ ] **Step 7: Run focused and package tests and verify GREEN**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./config ./component/outbound ./control -count=1
```

Expected: PASS for all three packages.

- [ ] **Step 8: Commit the configuration contract**

```bash
git add config/config.go config/failover_rotation_test.go \
  control/group_dialer_resolver.go control/group_dialer_resolver_test.go \
  component/outbound/failover_backoff.go \
  component/outbound/failover_controller.go \
  component/outbound/failover_roles.go component/outbound/failover_roles_test.go
git commit -m "feat(config): add failover probe backoff mode"
```

---

### Task 2: Apply One Delay Function to the Existing State Machine

**Files:**
- Modify: `component/outbound/failover_backoff.go`
- Modify: `component/outbound/failover_controller.go:645-716`
- Modify: `component/outbound/failover_rotation_test.go`
- Modify: `component/outbound/failover_rotation_integration_test.go`

**Interfaces:**
- Consumes: `FailoverRecoveryConfig`, the current delay, and the already-computed candidate-advance decision.
- Produces: `nextRecoveryProbeDelay(current time.Duration, cfg FailoverRecoveryConfig, candidateAdvanced bool) time.Duration`.
- Preserves: the single `onProbeFailureLocked`, `scheduleProbeLocked`, success, promotion, event, and notification paths.

- [ ] **Step 1: Write failing pure delay tests**

Add table-driven tests to `component/outbound/failover_rotation_test.go`:

```go
func TestNextRecoveryProbeDelay(t *testing.T) {
	base := FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax: 5 * time.Minute,
	}
	tests := []struct {
		name     string
		backoff  FailoverProbeBackoff
		current  time.Duration
		advanced bool
		want     time.Duration
	}{
		{"fixed same target", FailoverProbeBackoffFixed, 4 * time.Minute, false, 15 * time.Second},
		{"fixed advanced", FailoverProbeBackoffFixed, 4 * time.Minute, true, 15 * time.Second},
		{"exponential growth", FailoverProbeBackoffExponential, time.Minute, false, 2 * time.Minute},
		{"exponential cap crossing", FailoverProbeBackoffExponential, 4 * time.Minute, false, 5 * time.Minute},
		{"exponential remains capped", FailoverProbeBackoffExponential, 5 * time.Minute, false, 5 * time.Minute},
		{"exponential advanced", FailoverProbeBackoffExponential, 5 * time.Minute, true, 15 * time.Second},
		{"overflow safe", FailoverProbeBackoffExponential, time.Duration(1<<63 - 2), false, 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.Backoff = tt.backoff
			require.Equal(t, tt.want, nextRecoveryProbeDelay(tt.current, cfg, tt.advanced))
		})
	}
}
```

- [ ] **Step 2: Rewrite state-machine expectations for per-candidate reset**

Update `TestFailoverRotationFifthFailureAdvancesToB` so the first five A deadlines remain `15s, 45s, 105s, 225s, 465s`, but the next B deadline is `480s`, not `765s`:

```go
if got := nextAt.Sub(time.Unix(0, 0)); got != 480*time.Second {
	t.Fatalf("next probe deadline = %v, want 480s (B after reset initial)", got)
}
```

Update circular and probe-error tests to expect a reset delay of `ProbeInitial` immediately after advancement and normal exponential growth (`30s`, `1m`, `2m`, `4m`) within each new target. Change structured-log assertions so every `primary_rotation_started` and `recovery_target_advanced` event has `next_probe_in=15s`.

Add fixed-mode state-machine coverage that drives five failures on A and five on B, asserting every delta in scheduler history is 15 seconds, the sixth probe targets B, Fallback remains selected, and there is exactly one pending timer.

Extend `TestFailoverRotationCandidateCountAndZeroThreshold` and
`TestFailoverRotationZeroThresholdStillRecovers` with these assertions:

```go
// Single candidate + threshold: the fifth failure wraps to the same target,
// resets failedRecoveryProbes to zero, and schedules after ProbeInitial.
// Multiple candidates + attempts zero: fixed remains at ProbeInitial;
// exponential reaches ProbeMax; neither changes recoveryTarget.
```

- [ ] **Step 3: Run the focused tests and verify RED**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./component/outbound -run 'Test(NextRecoveryProbeDelay|FailoverRotation)' -count=1
```

Expected: FAIL because the helper does not exist and current rotation still carries capped delay into the next candidate.

- [ ] **Step 4: Implement overflow-safe unified delay calculation**

Add to `component/outbound/failover_backoff.go`:

```go
func nextRecoveryProbeDelay(
	current time.Duration,
	cfg FailoverRecoveryConfig,
	candidateAdvanced bool,
) time.Duration {
	if candidateAdvanced || cfg.Backoff == FailoverProbeBackoffFixed {
		return cfg.ProbeInitial
	}
	if current >= cfg.ProbeMax || current > cfg.ProbeMax/2 {
		return cfg.ProbeMax
	}
	return current * 2
}
```

Import `time` in the focused helper file. Validation guarantees positive exponential limits; do not duplicate validation or timer management here.

- [ ] **Step 5: Replace the inline backoff mutation after computing advancement**

In `onProbeFailureLocked`, retain the existing counter, confirmation reset, advancement condition, circular target update, snapshot publication, log choice, and single timer scheduling. Replace only the delay calculation and stale comment:

```go
advances := fc.config.RotationAttempts > 0 &&
	fc.failedRecoveryProbes >= fc.config.RotationAttempts
rotationStarts := advances && !fc.rotationActive

if advances {
	fc.rotationActive = true
	fc.recoveryTarget = (fc.recoveryTarget + 1) % len(fc.primaryCandidates)
	fc.failedRecoveryProbes = 0
}

fc.currentDelay = nextRecoveryProbeDelay(fc.currentDelay, fc.config, advances)
```

Calculate the delay after the candidate decision so advancement takes precedence. The existing logs will then report the actual reset delay without a separate logging branch.

- [ ] **Step 6: Cover confirmation failure and integration behavior**

Add a deterministic sequence in `failover_rotation_test.go` for each mode: one success sets confirmation delay to initial, then one failure clears confirmation. Assert fixed schedules initial; exponential schedules `2*initial` unless that failure reaches the attempt threshold, in which case it schedules initial on the next candidate.

Update `failover_rotation_integration_test.go` scripted sequences and comments that assume capped delay persists across candidates. Preserve target order, fixed Fallback selection until promotion, dynamic event content, notifier isolation, no standby probes, and one logical probe owner.

- [ ] **Step 7: Run outbound tests and verify GREEN**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./component/outbound -count=1
```

Expected: PASS for `github.com/daeuniverse/dae/component/outbound`.

- [ ] **Step 8: Commit the state-machine change**

```bash
git add component/outbound/failover_backoff.go \
  component/outbound/failover_controller.go \
  component/outbound/failover_rotation_test.go \
  component/outbound/failover_rotation_integration_test.go
git commit -m "fix(outbound): reset failover backoff per candidate"
```

---

### Task 3: Compare Effective Recovery Policy Across Reloads

**Files:**
- Modify: `component/outbound/failover_reload.go:15-130`
- Modify: `component/outbound/failover_reload_test.go`
- Modify: `control/group_dialer_resolver_test.go`

**Interfaces:**
- Produces: `equalFailoverRecoveryConfig(old, next FailoverRecoveryConfig) bool`.
- Consumes: normalized enum values already stored in both controllers.
- Preserves: mismatch precedence `fallback_changed > primary_candidates_changed > recovery_policy_changed` and existing transactional transfer/rollback ownership.

- [ ] **Step 1: Write failing effective-identity table tests**

Extend `TestFailoverReloadSnapshotResetsOnIdentityChange` and add a focused equality table:

```go
func TestEqualFailoverRecoveryConfigUsesEffectiveSemantics(t *testing.T) {
	base := baseReloadRecoveryConfig()
	tests := []struct {
		name   string
		modify func(*FailoverRecoveryConfig)
		want   bool
	}{
		{"identical exponential", func(*FailoverRecoveryConfig) {}, true},
		{"mode changed", func(c *FailoverRecoveryConfig) { c.Backoff = FailoverProbeBackoffFixed }, false},
		{"exponential max changed", func(c *FailoverRecoveryConfig) { c.ProbeMax *= 2 }, false},
		{"initial changed", func(c *FailoverRecoveryConfig) { c.ProbeInitial *= 2 }, false},
		{"successes changed", func(c *FailoverRecoveryConfig) { c.Successes++ }, false},
		{"stable time changed", func(c *FailoverRecoveryConfig) { c.StableTime *= 2 }, false},
		{"attempts changed", func(c *FailoverRecoveryConfig) { c.RotationAttempts++ }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := base
			tt.modify(&next)
			require.Equal(t, tt.want, equalFailoverRecoveryConfig(base, next))
		})
	}

	oldFixed := base
	oldFixed.Backoff = FailoverProbeBackoffFixed
	newFixed := oldFixed
	newFixed.ProbeMax = -time.Second
	require.True(t, equalFailoverRecoveryConfig(oldFixed, newFixed))
}
```

Add transfer-level assertions that fixed maximum-only changes preserve target, counter, confirmation, current delay, remaining delay, and one pending timer. Add mode-change and exponential-maximum cases expecting `recovery_policy_changed` while the old timer/generation/probe owner remains untouched.

- [ ] **Step 2: Extend invalid reload-build safety coverage**

Adapt `TestFailoverReloadInvalidConfigLeavesOldControllerActive` into table-driven invalid recovery cases:

```go
[]struct {
	name   string
	mutate func(*FailoverRecoveryConfig)
	want   string
}{
	{"invalid enum", func(c *FailoverRecoveryConfig) { c.Backoff = 255 }, "backoff enum"},
	{"exponential zero max", func(c *FailoverRecoveryConfig) { c.ProbeMax = 0 }, "must be positive"},
	{"exponential initial above max", func(c *FailoverRecoveryConfig) { c.ProbeInitial = 10 * time.Minute }, "must not exceed"},
}
```

For each case, capture the old snapshot, generation, pending count, active-probe token, and cancellation ownership before validating the replacement through `ResolveFailoverRoles`. Require validation to fail, then fire or release the old work and assert it continues exactly once.

In `control/group_dialer_resolver_test.go`, add `TestInvalidFailoverBackoffFailsBeforeDialerGroupConstruction`. Build a valid explicit-role `config.Group`, mutate its raw mode to `"linear"`, call `resolveConfiguredGroupDialers`, and assert the returned error contains the group name, received value, and accepted values. Do not call `NewDialerGroup` or `InheritDialerHealthFrom` after that error; this establishes the production build boundary, while the outbound test proves the old controller ownership mechanics at that boundary.

- [ ] **Step 3: Run reload tests and verify RED**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./component/outbound ./control -run 'Test(FailoverReload|EqualFailoverRecoveryConfig|ReloadInheritance)' -count=1
```

Expected: FAIL because raw `FailoverRecoveryConfig` equality treats fixed maximum-only changes as incompatible and the equality helper does not exist.

- [ ] **Step 4: Implement effective recovery comparison**

Add a focused comparator to `failover_reload.go`:

```go
func equalFailoverRecoveryConfig(old, next FailoverRecoveryConfig) bool {
	if old.Backoff != next.Backoff ||
		old.ProbeInitial != next.ProbeInitial ||
		old.Successes != next.Successes ||
		old.StableTime != next.StableTime ||
		old.RotationAttempts != next.RotationAttempts {
		return false
	}
	if old.Backoff == FailoverProbeBackoffExponential && old.ProbeMax != next.ProbeMax {
		return false
	}
	return true
}
```

Use it only in the recovery-policy branch of `compareFailoverReloadIdentity`:

```go
if !equalFailoverRecoveryConfig(old.Recovery, next.Recovery) {
	return false, "recovery_policy_changed"
}
```

Keep Fallback and ordered Primary checks first so mismatch reason precedence is unchanged.

- [ ] **Step 5: Run reload and race tests and verify GREEN**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./component/outbound ./control -run 'Test(FailoverReload|EqualFailoverRecoveryConfig|ReloadInheritance)' -count=1
orb env GOCACHE=/tmp/dae-go-cache go test -race -tags dae_stub_ebpf ./component/outbound ./control -count=1
```

Expected: both commands PASS with one logical probe owner across compatible transfer, incompatible build failure, commit, rollback, and stale completion.

- [ ] **Step 6: Commit effective reload identity**

```bash
git add component/outbound/failover_reload.go \
  component/outbound/failover_reload_test.go \
  control/group_dialer_resolver_test.go
git commit -m "fix(outbound): compare effective failover recovery policy"
```

---

### Task 4: Document Both Modes and Run Full Acceptance

**Files:**
- Modify: `config/desc.go:108-114`
- Modify: `example.dae:397-458`
- Modify: stale comments in `component/outbound/failover_controller.go`, `component/outbound/failover_reload.go`, `component/outbound/failover_rotation_test.go`, `component/outbound/failover_rotation_integration_test.go`, and `component/outbound/failover_reload_test.go`

**Interfaces:**
- Consumes: the shipped field names and exact runtime semantics from Tasks 1-3.
- Produces: user-facing descriptions and examples that distinguish fixed interval, exponential cap, and attempt-count rotation.
- Preserves: source-only rollout; no production gateway configuration mutation is part of this task.

- [ ] **Step 1: Add failing description/example searches**

Run before editing:

```bash
rg -n 'recovery_probe_backoff' config/desc.go example.dae
rg -n 'stays capped across rotation targets|Backoff stays capped|preserved across the threshold' \
  component/outbound config example.dae
```

Expected: the first command finds no shipped description/example; the second finds stale cross-candidate-cap statements.

- [ ] **Step 2: Update field descriptions**

Add and revise `GroupDesc` entries so they state exact responsibilities:

```go
"recovery_probe_backoff": "Recovery probe delay strategy (failover policy only): exponential or fixed. Default exponential. Both modes reset to recovery_probe_initial after candidate advancement; fixed always uses the initial interval.",
"recovery_probe_initial": "Delay before the first targeted recovery probe and confirmation probes (failover policy only). Default 15s. Fixed mode uses this for every probe; exponential mode doubles same-candidate failures from this value.",
"recovery_probe_max": "Maximum same-candidate delay in exponential recovery_probe_backoff mode (failover policy only). Default 5m. Ignored by fixed mode and never causes candidate advancement.",
"primary_rotation_attempts": `Consecutive failed recovery probes allowed for each current recovery target before advancing. 0 disables advancement. Delay strategy and recovery_probe_max do not change this threshold.`,
```

- [ ] **Step 3: Show one exponential and one clean fixed example**

Keep the single-Primary example exponential by omission and explain the default near its existing recovery fields. Update the rotating example to fixed mode and omit the unused maximum:

```dae
#proxy_failover_rotating {
#    primary: name(node_A, node_B, node_C)
#    fallback: name(xray_local)
#    policy: failover
#    recovery_probe_backoff: fixed
#    recovery_probe_initial: 15s
#    primary_rotation_attempts: 5
#    recovery_successes: 3
#    recovery_stable_time: 30s
#}
```

State in adjacent comments that fixed probes each candidate serially every 15 seconds, while five consecutive failures advance the cursor. Do not include `recovery_probe_max` in the fixed example.

- [ ] **Step 4: Remove stale comments without rewriting history**

Update active source and tests to say exponential growth is per candidate and advancement resets to initial. Do not edit historical design documents or old review reports merely because they describe previous behavior; restrict stale-search enforcement to active source, current tests, `config/desc.go`, and `example.dae`.

Run:

```bash
rg -n 'stays capped across rotation targets|Backoff stays capped|preserved across the threshold' \
  component/outbound config example.dae
```

Expected: no matches.

- [ ] **Step 5: Format and run focused tests**

Run:

```bash
gofmt -w config/config.go config/failover_rotation_test.go \
  control/group_dialer_resolver.go control/group_dialer_resolver_test.go \
  component/outbound/failover_backoff.go \
  component/outbound/failover_controller.go component/outbound/failover_roles.go \
  component/outbound/failover_roles_test.go \
  component/outbound/failover_rotation_test.go \
  component/outbound/failover_rotation_integration_test.go \
  component/outbound/failover_reload.go component/outbound/failover_reload_test.go

orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf \
  ./config ./component/outbound ./control ./cmd -count=1
```

Expected: focused packages PASS.

- [ ] **Step 6: Run race and complete Linux tests**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -race -tags dae_stub_ebpf \
  ./component/outbound ./control -count=1
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./... -count=1
```

Expected: both commands PASS.

- [ ] **Step 7: Run repository hygiene and architecture checks**

Run:

```bash
git diff --check
rg -n '^(<<<<<<<|=======|>>>>>>>)' \
  config control component/outbound example.dae
sed -n '/[[:blank:]]$/p' config/desc.go example.dae \
  component/outbound/failover_backoff.go \
  component/outbound/failover_controller.go \
  component/outbound/failover_reload.go
rg -n 'type .*Backoff.*interface|type .*Fixed.*Controller|go .*probe|go .*Probe' \
  component/outbound
git status --short
```

Expected: no whitespace/conflict-marker findings; no strategy interface, second fixed controller, or new parallel probe goroutine; status contains only planned files.

- [ ] **Step 8: Commit documentation and final test adjustments**

```bash
git add config/desc.go example.dae \
  component/outbound/failover_controller.go \
  component/outbound/failover_reload.go \
  component/outbound/failover_rotation_test.go \
  component/outbound/failover_rotation_integration_test.go \
  component/outbound/failover_reload_test.go
git commit -m "docs(config): document failover probe backoff"
```

- [ ] **Step 9: Record final branch evidence without changing production**

Run:

```bash
git status --short
git log --oneline --decorate gateway/main..HEAD
git diff --stat gateway/main...HEAD
```

Expected: clean worktree; four feature-grouped implementation commits after the two approved design commits; no operations-repository or live gateway configuration changes.
