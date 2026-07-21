# Failover Primary Rotation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in ordered Primary rotation mechanism to DAE's existing two-role failover policy while keeping one current Primary, one fixed Fallback, serial recovery probing, and memory-only runtime state.

**Architecture:** Extend failover configuration so priorities `0, 2, 3...` form an ordered Primary candidate list and priority `1` remains the fixed Fallback. Keep ordinary selection on one immutable atomic snapshot; move candidate traversal, failed-probe counting, promotion, logging, and warm-reload state transfer into the existing failover controller. Warm reload uses a transactional in-memory transfer so an in-flight probe is owned by one generation, while full process restart always starts at priority `0`.

**Tech Stack:** Go 1.26+, DAE custom config DSL and mapstructure decoder, `component/outbound/dialer`, Logrus structured logging, OrbStack Ubuntu 24.04 arm64 with `dae_stub_ebpf`, production gateway `vm-ubuntu-agent` on Linux amd64.

## Global Constraints

- `primary_rotation_attempts` defaults to `0`; the existing two-dialer failover behavior must remain unchanged when it is omitted or zero.
- Priority `0` is the initial Primary, priority `1` is the fixed Fallback, and unique priorities `2+` are standby Primary candidates sorted numerically with gaps allowed.
- The production setting is `primary_rotation_attempts: 5`, `recovery_probe_initial: 15s`, `recovery_probe_max: 5m`, `recovery_successes: 3`, and `recovery_stable_time: 30s`.
- The fifth failed recovery probe occurs at logical offset `7m45s`; the next candidate is probed at `12m45s` because the existing 5-minute capped backoff is preserved.
- Any successful target remains the sole recovery target until it either fails or satisfies both the consecutive-success and stable-time requirements.
- Ordinary TCP/UDP selection must perform one atomic snapshot load without candidate traversal, controller locking, timer work, or health work.
- Only the current Primary's confirmed TCP-unavailable transition may trigger failover; UDP and standby-candidate transitions must not.
- At most one recovery timer and one in-flight recovery probe may exist per failover group.
- Rotation state is memory-only: do not modify configuration or create a state file; warm reload may inherit compatible state, but process restart starts from priority `0`.
- Warm reload compatibility requires identical fixed Fallback, ordered Primary identities, `primary_rotation_attempts`, `recovery_probe_initial`, `recovery_probe_max`, `recovery_successes`, and `recovery_stable_time`.
- Candidate advancement and probe errors are debug-level structured logs and must not emit Bark; only `failover_switch` and `failback_complete` remain notification events.
- Do not intentionally break the production Primary to test rotation without separate user authorization.
- Run all Git commands with explicit repository/worktree paths; keep the operations repository and DAE source repository changes in separate commits.

---

## File Structure

### Production files

- `config/config.go`: decode the new flat group-local integer setting.
- `config/desc.go`: expose failover/rotation semantics in generated configuration help.
- `component/outbound/dialer_selection_policy.go`: reject invalid rotation settings on both startup and reload construction paths.
- `cmd/validate.go`: reject negative attempts and rotation settings on non-failover policies before activation.
- `component/outbound/dialer_group.go`: validate/sort failover roles, build the candidate list, and keep ordinary selection on the atomic active dialer.
- `component/outbound/failover_controller.go`: own dynamic Primary identity, recovery target, serial probe state, promotion, events, logs, and snapshot fields.
- `component/outbound/failover_reload.go`: isolate reload identity comparison and transactional pause/restore/rollback behavior from the core state machine.
- `control/control_plane.go`: pass the decoded setting into failover construction and aggregate per-group reload transfers.
- `cmd/reload_manager.go`: retain the reload transfer transaction across staged handoff.
- `cmd/run.go`: commit the transfer on successful cutover and roll it back if staged reload fails.
- `example.dae`: document the opt-in configuration and restart/reload behavior.

### Test files

- `config/failover_rotation_test.go`: decoder default and explicit-value tests.
- `cmd/validate_failover_rotation_test.go`: validate-stage policy/value tests.
- `component/outbound/failover_rotation_test.go`: deterministic controller timing, cursor, confirmation, probe-error, promotion, and log tests.
- `component/outbound/failover_rotation_integration_test.go`: DialerGroup selection, exclusion, standby-idle, event, and future-failure integration tests.
- `component/outbound/failover_reload_test.go`: identity compatibility, snapshot fields, in-flight transfer, restart reset, and transaction rollback tests.
- `control/control_plane_failover_reload_test.go`: multi-group reload aggregation and reset-reason logging tests.
- `control/control_plane_drain_test.go`: adapt existing inheritance tests to the transactional return type.
- `cmd/run_failover_reload_test.go`: staged handoff commit/rollback wiring tests.

---

### Task 1: Add and validate the configuration surface

**Files:**
- Create: `config/failover_rotation_test.go`
- Create: `cmd/validate_failover_rotation_test.go`
- Modify: `config/config.go:125-160`
- Modify: `component/outbound/dialer_selection_policy.go:20-65`
- Modify: `component/outbound/dialer_selection_policy_test.go:1-80`
- Modify: `cmd/validate.go:80-155`

**Interfaces:**
- Produces: `config.Group.PrimaryRotationAttempts int` with `mapstructure:"primary_rotation_attempts"` and default `0`.
- Produces: runtime policy validation in `NewDialerSelectionPolicyFromGroupParam`, so startup/reload rejects negative attempts or attempts on non-failover policies even if the operator did not run `dae validate` first.
- Produces: `validateFailoverSettings(*config.Config) error`, called by `dae validate` after policy parsing.
- Consumes: `outbound.NewDialerSelectionPolicyFromGroupParam(*config.Group)` and returns its setting-specific error from `dae validate`.

- [ ] **Step 1: Write decoder tests for the default and explicit value**

Create `config/failover_rotation_test.go`:

```go
package config

import (
	"testing"

	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

func decodeRotationGroup(t *testing.T, setting string) Group {
	t.Helper()
	raw := `
global {}
routing { fallback: direct }
group {
  proxy_failover {
    filter: name(A) [priority: 0]
    filter: name(xray_local) [priority: 1]
    policy: failover
` + setting + `
  }
}`
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	conf, err := New(sections)
	require.NoError(t, err)
	require.Len(t, conf.Group, 1)
	return conf.Group[0]
}

func TestGroupPrimaryRotationAttemptsDefaultsToZero(t *testing.T) {
	g := decodeRotationGroup(t, "")
	require.Equal(t, 0, g.PrimaryRotationAttempts)
}

func TestGroupPrimaryRotationAttemptsDecodesExplicitValue(t *testing.T) {
	g := decodeRotationGroup(t, "primary_rotation_attempts: 5")
	require.Equal(t, 5, g.PrimaryRotationAttempts)
}
```

- [ ] **Step 2: Run the decoder tests and verify RED**

Run in OrbStack:

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./config -run PrimaryRotationAttempts -count=1'
```

Expected: FAIL because `config.Group` has no `PrimaryRotationAttempts` field.

- [ ] **Step 3: Add the decoded field**

Add this field beside the existing failover recovery settings in `config/config.go`:

```go
	// PrimaryRotationAttempts enables ordered standby-Primary recovery after
	// this many failed probes against the current Primary. Zero disables it.
	PrimaryRotationAttempts int `mapstructure:"primary_rotation_attempts" default:"0"`
```

- [ ] **Step 4: Write validate-stage setting tests**

Create `cmd/validate_failover_rotation_test.go`:

```go
package cmd

import (
	"testing"

	"github.com/daeuniverse/dae/config"
	"github.com/stretchr/testify/require"
)

func TestValidateFailoverSettings(t *testing.T) {
	tests := []struct {
		name    string
		group   config.Group
		wantErr string
	}{
		{name: "disabled failover", group: config.Group{Name: "g", Policy: "failover"}},
		{name: "enabled failover", group: config.Group{Name: "g", Policy: "failover", PrimaryRotationAttempts: 5}},
		{name: "negative", group: config.Group{Name: "g", Policy: "failover", PrimaryRotationAttempts: -1}, wantErr: "must not be negative"},
		{name: "non failover", group: config.Group{Name: "g", Policy: "random", PrimaryRotationAttempts: 5}, wantErr: "requires policy: failover"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := &config.Config{Group: []config.Group{tt.group}}
			err := validateFailoverSettings(conf)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}
```

Extend `component/outbound/dialer_selection_policy_test.go` with the same negative and non-failover cases, plus a positive failover case. These tests prove the daemon's startup/reload construction path rejects invalid values independently of the `dae validate` command.

- [ ] **Step 5: Run the validate-stage test and verify RED**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./cmd -run ValidateFailoverSettings -count=1'
```

Expected: FAIL with `undefined: validateFailoverSettings`.

- [ ] **Step 6: Implement validation and wire it into `dae validate`**

Add to `cmd/validate.go`:

```go
func validateFailoverSettings(conf *config.Config) error {
	for i := range conf.Group {
		g := &conf.Group[i]
		if g.PrimaryRotationAttempts == 0 {
			continue
		}
		if _, err := outbound.NewDialerSelectionPolicyFromGroupParam(g); err != nil {
			return fmt.Errorf("group %q: %w", g.Name, err)
		}
	}
	return nil
}
```

In `NewDialerSelectionPolicyFromGroupParam`, after parsing the single policy function and before returning its policy, reject `PrimaryRotationAttempts < 0`, then reject any nonzero value unless `f.Name` is `failover`. Use the exact error fragments asserted above: `must not be negative` and `requires policy: failover`.

Call it in the validate command immediately before `validateFailoverNotify(conf)` and use the same collector error path with result name `failover_settings_error`.

- [ ] **Step 7: Run focused config/cmd tests and verify GREEN**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./config ./component/outbound ./cmd -run "PrimaryRotationAttempts|ValidateFailoverSettings|ValidateFailoverNotify|DialerSelectionPolicy.*Rotation" -count=1'
```

Expected: PASS for both packages.

- [ ] **Step 8: Commit the configuration slice**

```bash
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation add config/config.go config/failover_rotation_test.go component/outbound/dialer_selection_policy.go component/outbound/dialer_selection_policy_test.go cmd/validate.go cmd/validate_failover_rotation_test.go
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation commit -m "feat(config): add failover primary rotation setting"
```

---

### Task 2: Build ordered failover roles and preserve the atomic selection hot path

**Files:**
- Modify: `component/outbound/dialer_group.go:20-115,175-210,525-565`
- Modify: `component/outbound/failover_controller.go:20-125,360-375`
- Modify: `component/outbound/failover_controller_test.go:244-410,698-725`
- Modify: `control/control_plane.go:800-815`
- Create: `component/outbound/failover_rotation_integration_test.go`

**Interfaces:**
- Produces: `FailoverRecoveryConfig.RotationAttempts int`.
- Produces: `FailoverConfig.PrimaryCandidateIdxs []int`, ordered by numeric priority and including priority `0` at position zero.
- Produces: `NewFailoverControllerWithCandidates(log, groupName, primaryCandidates, fallback, config) *FailoverController`.
- Produces: `FailoverController.ActiveDialer() (*dialer.Dialer, bool)`, where the boolean is true only when the fixed Fallback role is active.
- Preserves: `NewFailoverController(...)` and `ActiveDialerIndex()` as legacy test/caller adapters.

Create `component/outbound/failover_rotation_integration_test.go` with `package outbound` and imports for `testing`, `time`, `consts`, and `dialer`, then add these shared helpers; package-level tests in `component/outbound` can reuse them. Add `reflect` to `failover_controller_test.go` for the ordered-index assertion:

```go
type rotationTestNodes struct {
	A        *dialer.Dialer
	B        *dialer.Dialer
	C        *dialer.Dialer
	Fallback *dialer.Dialer
}

func testFailoverDialerOption() *dialer.GlobalOption {
	return &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     time.Hour,
		CheckTolerance:    0,
	}
}

func newNamedDirectDialer(option *dialer.GlobalOption, name string) *dialer.Dialer {
	d := newDirectDialer(option, false)
	d.Property().Name = name
	return d
}

func newRotationIntegrationGroup(t *testing.T) (*DialerGroup, rotationTestNodes) {
	t.Helper()
	option := testFailoverDialerOption()
	nodes := rotationTestNodes{
		A:        newNamedDirectDialer(option, "A"),
		B:        newNamedDirectDialer(option, "B"),
		C:        newNamedDirectDialer(option, "C"),
		Fallback: newNamedDirectDialer(option, "fallback"),
	}
	dialers := []*dialer.Dialer{nodes.Fallback, nodes.C, nodes.A, nodes.B}
	annotations := []*dialer.Annotation{
		{Priority: 1},
		{Priority: 3},
		{Priority: 0},
		{Priority: 2},
	}
	cfg, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	group := NewDialerGroup(
		option,
		"rotation-test",
		dialers,
		annotations,
		DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
		func(bool, *dialer.NetworkType, bool) {},
		cfg,
	)
	t.Cleanup(func() { _ = group.Close() })
	return group, nodes
}
```

- [ ] **Step 1: Replace the old three-dialer rejection test with ordered-role table tests**

Add table cases to `component/outbound/failover_controller_test.go` that call `ValidateFailoverGroup` with `RotationAttempts` embedded in `FailoverRecoveryConfig`:

```go
func TestValidateFailoverGroupRotationRoles(t *testing.T) {
	option := testFailoverDialerOption()
	dialers := []*dialer.Dialer{
		newNamedDirectDialer(option, "fallback"),
		newNamedDirectDialer(option, "candidate-4"),
		newNamedDirectDialer(option, "initial"),
		newNamedDirectDialer(option, "candidate-2"),
	}
	annotations := []*dialer.Annotation{
		{Priority: 1},
		{Priority: 4},
		{Priority: 0},
		{Priority: 2},
	}
	cfg, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.PrimaryCandidateIdxs, []int{2, 3, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate indices = %v, want %v", got, want)
	}
	if cfg.FallbackIdx != 0 {
		t.Fatalf("fallback index = %d, want 0", cfg.FallbackIdx)
	}
}
```

Also add named negative cases for rotation disabled with priority `2`, enabled without a candidate, negative `RotationAttempts`, duplicate priority, missing annotation, negative priority, and duplicate dialer pointer.

- [ ] **Step 2: Run role validation tests and verify RED**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./component/outbound -run "ValidateFailoverGroupRotation|ValidateFailoverGroup_" -count=1'
```

Expected: FAIL because priorities `2+`, `RotationAttempts`, and `PrimaryCandidateIdxs` are unsupported.

- [ ] **Step 3: Implement deterministic role validation**

Change the configuration types in `component/outbound/dialer_group.go`:

```go
type FailoverConfig struct {
	PrimaryCandidateIdxs []int
	FallbackIdx          int
	Recovery             FailoverRecoveryConfig
}
```

Add `RotationAttempts int` to `FailoverRecoveryConfig`. In `ValidateFailoverGroup`, reject a negative value, collect `(priority, dialerIndex)` pairs, reject duplicate priorities and duplicate dialer pointers, enforce the legacy two-role shape when `RotationAttempts == 0`, enforce at least one standby when it is positive, sort Primary pairs by priority, and return indices in that sorted order. Do not require priorities to be contiguous.

Use this exact sort boundary:

```go
sort.Slice(primaryRoles, func(i, j int) bool {
	return primaryRoles[i].priority < primaryRoles[j].priority
})
```

- [ ] **Step 4: Add a candidate-aware controller constructor without breaking legacy tests**

In `component/outbound/failover_controller.go`, add:

```go
func NewFailoverControllerWithCandidates(
	log *logrus.Logger,
	groupName string,
	primaryCandidates []*dialer.Dialer,
	fallback *dialer.Dialer,
	config FailoverRecoveryConfig,
) *FailoverController
```

Keep the existing constructor as:

```go
func NewFailoverController(
	log *logrus.Logger,
	groupName string,
	primary *dialer.Dialer,
	fallback *dialer.Dialer,
	config FailoverRecoveryConfig,
) *FailoverController {
	return NewFailoverControllerWithCandidates(log, groupName, []*dialer.Dialer{primary}, fallback, config)
}
```

Store `primaryCandidates`, `currentPrimary`, and `recoveryTarget`, initialized to candidate position `0`.

- [ ] **Step 5: Make the atomic snapshot carry the selected dialer pointer**

Change the immutable snapshot to:

```go
type failoverSnapshot struct {
	state         failoverState
	activeDialer  *dialer.Dialer
	usingFallback bool
}

func (fc *FailoverController) ActiveDialer() (*dialer.Dialer, bool) {
	snap := fc.snapshot.Load()
	return snap.activeDialer, snap.usingFallback
}
```

Keep `ActiveDialerIndex()` returning legacy role `0` for current Primary and `1` for Fallback. Update `publishSnapshot()` to store either `primaryCandidates[currentPrimary]` or the fixed Fallback.

- [ ] **Step 6: Update DialerGroup construction and selection**

In `NewDialerGroup`, map `PrimaryCandidateIdxs` to a candidate slice and call `NewFailoverControllerWithCandidates`. Mark and activate every Primary candidate's event-driven connectivity worker; do not create an `AliveDialerSet` for failover.

Replace the failover branch in `_select` with one atomic result:

```go
	d, usingFallback := g.failoverController.ActiveDialer()
	if d == nil {
		return nil, 0, nil, fmt.Errorf("failover active dialer is nil")
	}
	if excluded != nil && d == excluded {
		if !usingFallback {
			fallback := g.Dialers[g.failoverCfg.FallbackIdx]
			if fallback != excluded {
				return fallback, 0, preferAlternateSelectionNetworkType(fallback, networkType), nil
			}
		}
		return nil, 0, nil, ErrNoAliveDialer
	}
	return d, 0, preferAlternateSelectionNetworkType(d, networkType), nil
```

- [ ] **Step 7: Pass the setting from the control plane**

Set `RotationAttempts: group.PrimaryRotationAttempts` when building `FailoverRecoveryConfig` in `control/control_plane.go`.

- [ ] **Step 8: Add hot-path and standby-idle integration tests**

In `component/outbound/failover_rotation_integration_test.go`, build A/Fallback/B/C in shuffled dialer order and verify:

```go
func TestFailoverRotationInitialSelectionAndExclusion(t *testing.T) {
	group, nodes := newRotationIntegrationGroup(t)
	d, _, _, err := group.SelectWithExclusionResult(TestNetworkType, false, nil)
	if err != nil || d != nodes.A {
		t.Fatalf("initial selection = %v, %v; want A", d, err)
	}
	d, _, _, err = group.SelectWithExclusionResult(TestNetworkType, false, nodes.A)
	if err != nil || d != nodes.Fallback {
		t.Fatalf("excluded A selection = %v, %v; want fallback", d, err)
	}
}
```

Add an idle test with `CheckInterval: 20ms`, wait `100ms`, and assert no standby probe hook fired and `group.currentSelectionState().aliveDialerSets[0] == nil`.

- [ ] **Step 9: Run focused group tests and verify GREEN**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./component/outbound ./control -run "FailoverGroup|FailoverRotationInitial|PrimaryActive_NoPeriodic|ValidateFailoverGroup" -count=1'
```

Expected: PASS.

- [ ] **Step 10: Commit the role model and hot path**

```bash
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation add component/outbound/dialer_group.go component/outbound/failover_controller.go component/outbound/failover_controller_test.go component/outbound/failover_rotation_integration_test.go control/control_plane.go
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation commit -m "feat(outbound): model ordered failover primaries"
```

---

### Task 3: Implement deterministic serial rotation and stable promotion

**Files:**
- Create: `component/outbound/failover_rotation_test.go`
- Modify: `component/outbound/failover_controller.go:35-465`

**Interfaces:**
- Consumes: ordered `primaryCandidates`, `currentPrimary`, `recoveryTarget`, and `FailoverRecoveryConfig.RotationAttempts` from Task 2.
- Produces: `failedRecoveryProbes int`, `rotationActive bool`, serial target advancement, and candidate promotion.
- Produces: test-only injectable `failoverScheduler` and target-aware `probeTCP func(context.Context, *dialer.Dialer) (bool, error)`.

- [ ] **Step 1: Add a deterministic scheduler used by controller tests**

Create `component/outbound/failover_rotation_test.go` with `package outbound` and initial imports for `testing` and `time`; add `context`, `errors`, and `dialer` in the later steps when their probe cases are introduced.

Define the production boundary in `failover_controller.go`:

```go
type failoverTimer interface {
	Stop() bool
}

type failoverScheduler interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) failoverTimer
}

type systemFailoverScheduler struct{}

func (systemFailoverScheduler) Now() time.Time { return time.Now() }
func (systemFailoverScheduler) AfterFunc(d time.Duration, fn func()) failoverTimer {
	return time.AfterFunc(d, fn)
}
```

Create this fake scheduler in `component/outbound/failover_rotation_test.go`; keep `history` separate from pending calls so tests can assert every absolute logical deadline after timers fire:

```go
type fakeFailoverTimer struct {
	stopped bool
	fn      func()
}

func (t *fakeFailoverTimer) Stop() bool {
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

type scheduledFailoverCall struct {
	at    time.Time
	timer *fakeFailoverTimer
}

type fakeFailoverScheduler struct {
	now     time.Time
	pending []scheduledFailoverCall
	history []time.Time
}

func newFakeFailoverScheduler() *fakeFailoverScheduler {
	return &fakeFailoverScheduler{now: time.Unix(0, 0)}
}

func (s *fakeFailoverScheduler) Now() time.Time { return s.now }

func (s *fakeFailoverScheduler) AfterFunc(d time.Duration, fn func()) failoverTimer {
	timer := &fakeFailoverTimer{fn: fn}
	at := s.now.Add(d)
	s.pending = append(s.pending, scheduledFailoverCall{at: at, timer: timer})
	s.history = append(s.history, at)
	return timer
}

func (s *fakeFailoverScheduler) Advance(d time.Duration) { s.now = s.now.Add(d) }

func (s *fakeFailoverScheduler) FireNext(t *testing.T) {
	t.Helper()
	for len(s.pending) > 0 {
		call := s.pending[0]
		s.pending = s.pending[1:]
		if call.timer.stopped {
			continue
		}
		if call.at > s.now {
			s.now = call.at
		}
		call.timer.stopped = true
		call.timer.fn()
		return
	}
	t.Fatal("no pending failover timer")
}
```

- [ ] **Step 2: Write the five-failure timing test**

Add `TestFailoverRotationFifthFailureAdvancesToB` using A/B/C/Fallback, an all-failure target-aware probe, and the fake scheduler. Assert target/deadline pairs:

```go
want := []struct {
	target string
	at     time.Duration
}{
	{target: "A", at: 15 * time.Second},
	{target: "A", at: 45 * time.Second},
	{target: "A", at: 105 * time.Second},
	{target: "A", at: 225 * time.Second},
	{target: "A", at: 465 * time.Second},
	{target: "B", at: 765 * time.Second},
}
```

The test must assert `failedRecoveryProbes == 5`, `rotationActive == true`, `recoveryTarget == 1`, active dialer remains Fallback, and maximum in-flight probes equals one.

- [ ] **Step 3: Run the timing test and verify RED**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./component/outbound -run FailoverRotationFifthFailureAdvancesToB -count=1'
```

Expected: FAIL because the controller still probes only its initial Primary and does not count episode failures.

- [ ] **Step 4: Implement target-aware scheduling and failure accounting**

Change the probe function and mutable state:

```go
	probeTCP func(context.Context, *dialer.Dialer) (bool, error)
	scheduler failoverScheduler

	currentPrimary       int
	recoveryTarget       int
	rotationActive       bool
	failedRecoveryProbes int
```

In `runProbe`, capture the current target while holding `mu`, release the mutex for network I/O, then reject stale generations before applying the result. On every non-cancellation failed result:

```go
	fc.failedRecoveryProbes++
	fc.recoverySuccesses = 0
	fc.stableSince = time.Time{}
	fc.currentDelay = minDuration(fc.currentDelay*2, fc.config.ProbeMax)
	if fc.rotationActive {
		fc.recoveryTarget = (fc.recoveryTarget + 1) % len(fc.primaryCandidates)
	} else if fc.config.RotationAttempts > 0 && fc.failedRecoveryProbes >= fc.config.RotationAttempts {
		fc.rotationActive = true
		fc.recoveryTarget = (fc.currentPrimary + 1) % len(fc.primaryCandidates)
	}
```

Add the local helper beside the scheduler boundary:

```go
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
```

Schedule exactly one next probe with the preserved delay.

- [ ] **Step 5: Write circular scan and probe-error tests**

Add:

- `TestFailoverRotationCircularTargets`: after the threshold, failures visit B, C, A, B.
- `TestFailoverRotationProbeErrorMatchesFalseResult`: an error on A failure five and an error during B confirmation produce the same count, target, cleared confirmation fields, and delay as `ok=false, err=nil`.
- `TestFailoverRotationCancellationDoesNotConsumeFailure`: `context.Canceled` during close leaves count/cursor unchanged.

Use table-driven expected snapshots rather than sleeps.

- [ ] **Step 6: Write both-condition recovery tests**

Add four deterministic cases:

```go
tests := []struct {
	name          string
	results       []bool
	elapsed       time.Duration
	wantPromoted  bool
	wantSuccesses int
}{
	{name: "three successes before stable time", results: []bool{true, true, true}, elapsed: 29 * time.Second, wantPromoted: false, wantSuccesses: 3},
	{name: "stable time with two successes", results: []bool{true, true}, elapsed: 30 * time.Second, wantPromoted: false, wantSuccesses: 2},
	{name: "failure resets confirmation", results: []bool{true, true, false, true}, elapsed: 45 * time.Second, wantPromoted: false, wantSuccesses: 1},
	{name: "both conditions", results: []bool{true, true, true}, elapsed: 30 * time.Second, wantPromoted: true, wantSuccesses: 0},
}
```

Advance the fake scheduler's logical clock between results and assert both `recoverySuccesses` and `stableSince` boundaries.

- [ ] **Step 7: Implement success confirmation and promotion**

Use `scheduler.Now()` everywhere the controller currently uses `time.Now()` or `time.Since`. On qualification:

```go
	promoted := fc.recoveryTarget
	fc.currentPrimary = promoted
	fc.state = statePrimaryActive
	fc.rotationActive = false
	fc.failedRecoveryProbes = 0
	fc.recoverySuccesses = 0
	fc.stableSince = time.Time{}
	fc.currentDelay = fc.config.ProbeInitial
	fc.publishSnapshot()
```

Do not inspect or preempt to an earlier candidate after promotion.

- [ ] **Step 8: Run all rotation state-machine tests and verify GREEN**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./component/outbound -run "FailoverRotation|FailoverController_Backoff|StableTimeRequired|ExplicitProbe" -count=1'
```

Expected: PASS with no real-time sleeps in the new rotation test file.

- [ ] **Step 9: Commit the rotation state machine**

```bash
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation add component/outbound/failover_controller.go component/outbound/failover_rotation_test.go
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation commit -m "feat(outbound): rotate failed primary recovery targets"
```

---

### Task 4: Wire dynamic Primary health, events, logs, and future failures

**Files:**
- Modify: `component/outbound/failover_controller.go`
- Modify: `component/outbound/failover_event.go:15-45`
- Modify: `component/outbound/failover_rotation_test.go`
- Modify: `component/outbound/failover_rotation_integration_test.go`

**Interfaces:**
- Consumes: candidate position state and promotion from Task 3.
- Produces: `onCandidateHealthChange(candidate int, networkType *dialer.NetworkType, alive bool)`.
- Produces: dynamic `FailoverEvent.Primary`, structured `primary_rotation_started`, and `recovery_target_advanced` fields.
- Preserves: event types `failover_switch` and `failback_complete`; no new notifier event type.

- [ ] **Step 1: Write dynamic health and sticky-promotion integration tests**

First add `context` to the imports of `component/outbound/failover_rotation_integration_test.go`, then add these package-local helpers. They use the deterministic scheduler from Task 3 and verify the expected target on every probe:

```go
type scriptedProbeResult struct {
	target *dialer.Dialer
	ok     bool
	err    error
}

func installRotationTestScheduler(group *DialerGroup) *fakeFailoverScheduler {
	scheduler := newFakeFailoverScheduler()
	group.failoverController.scheduler = scheduler
	return scheduler
}

func triggerCandidateHealth(group *DialerGroup, candidate int, networkType *dialer.NetworkType, alive bool) {
	group.failoverController.onCandidateHealthChange(candidate, networkType, alive)
}

func driveProbeResults(
	t *testing.T,
	group *DialerGroup,
	scheduler *fakeFailoverScheduler,
	results ...scriptedProbeResult,
) {
	t.Helper()
	next := 0
	group.failoverController.probeTCP = func(_ context.Context, target *dialer.Dialer) (bool, error) {
		if next >= len(results) {
			t.Fatalf("unexpected probe of %s", target.Property().Name)
		}
		result := results[next]
		next++
		if target != result.target {
			t.Fatalf("probe target = %s, want %s", target.Property().Name, result.target.Property().Name)
		}
		return result.ok, result.err
	}
	for range results {
		scheduler.FireNext(t)
	}
	if next != len(results) {
		t.Fatalf("consumed %d probe results, want %d", next, len(results))
	}
}

func selectedDialer(t *testing.T, group *DialerGroup) *dialer.Dialer {
	t.Helper()
	d, _, _, err := group.SelectWithExclusionResult(TestNetworkType, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
```

Add these scenarios, using three B successes at the existing 15-second confirmation cadence so the first-to-third success span is exactly 30 seconds:

```go
func TestFailoverRotationPromotedBIsSticky(t *testing.T) {
	group, nodes := newRotationIntegrationGroup(t)
	scheduler := installRotationTestScheduler(group)
	triggerCandidateHealth(group, 0, TestNetworkType, false)
	driveProbeResults(t, group, scheduler,
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.B, ok: true},
		scriptedProbeResult{target: nodes.B, ok: true},
		scriptedProbeResult{target: nodes.B, ok: true},
	)
	if d := selectedDialer(t, group); d != nodes.B {
		t.Fatalf("selected %v, want promoted B", d)
	}
	triggerCandidateHealth(group, 0, TestNetworkType, true)
	if d := selectedDialer(t, group); d != nodes.B {
		t.Fatalf("A preempted B: selected %v", d)
	}
}
```

Add `TestFailoverRotationPromotedBFailureStartsAtC`: after B promotion and B TCP failure, assert five B probes followed by C. Add UDP and non-current-candidate failure cases that leave B active.

- [ ] **Step 2: Run dynamic health tests and verify RED**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./component/outbound -run "PromotedB|NonCurrent|UDP" -count=1'
```

Expected: FAIL because callbacks still refer to the initial Primary.

- [ ] **Step 3: Register identity-aware callbacks for every candidate**

During construction:

```go
for i, candidate := range fc.primaryCandidates {
	idx := i
	candidate.RegisterAliveTransitionCallback(func(nt *dialer.NetworkType, alive bool) {
		fc.onCandidateHealthChange(idx, nt, alive)
	})
	candidate.MarkKeepConnectivityCheck()
}
```

`onCandidateHealthChange` must return unless TCP, unavailable, controller open, state is `primary_active`, and `candidate == currentPrimary`. Keep a legacy `onPrimaryHealthChange` test helper that delegates to the current candidate.

- [ ] **Step 4: Write structured log and dynamic event tests**

Attach a Logrus test hook and the existing recording callback. Assert:

- `failover_switch` after B is current names B and Fallback.
- `primary_rotation_started` is debug-level with `failed_primary=A`, `from_target=A`, `to_target=B`, `failed_attempts=5`, and the actual `next_probe_in`.
- Later B-to-C and C-to-A advances keep `failed_primary=A`, increment `failed_attempts`, and report the actual cursor edge.
- `failback_complete` names the promoted candidate.
- Candidate movement emits no additional `FailoverEvent`.

- [ ] **Step 5: Implement dynamic event construction and logs**

Use helpers that derive names under the controller mutex:

```go
func (fc *FailoverController) currentPrimaryDialerLocked() *dialer.Dialer {
	return fc.primaryCandidates[fc.currentPrimary]
}

func (fc *FailoverController) recoveryTargetDialerLocked() *dialer.Dialer {
	return fc.primaryCandidates[fc.recoveryTarget]
}
```

Emit `primary_rotation_started` on the threshold transition and `recovery_target_advanced` on every later cursor edge. Both use `.Debug(...)`; ordinary probe errors remain debug-level. Keep Bark dispatch only in `emitEventLocked` for switch and completed failback.

- [ ] **Step 6: Prove notifier isolation**

Install a `FailoverEventCallback` whose `OnFailoverEvent` records then returns without mutating the controller, and a dispatcher test whose notifier function fails. Compare selection, counter, cursor, timer count, and subsequent promotion with a no-op notifier run.

- [ ] **Step 7: Run event, log, and integration tests**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./component/outbound ./component/notifier ./control -run "FailoverRotation|FailoverEvent|Bark" -count=1'
```

Expected: PASS; event counts remain one switch plus one completed failback.

- [ ] **Step 8: Commit dynamic Primary behavior**

```bash
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation add component/outbound/failover_controller.go component/outbound/failover_event.go component/outbound/failover_rotation_test.go component/outbound/failover_rotation_integration_test.go
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation commit -m "feat(outbound): promote dynamic failover primaries"
```

---

### Task 5: Make warm reload transfer transactional and restart non-persistent

**Files:**
- Create: `component/outbound/failover_reload.go`
- Create: `component/outbound/failover_reload_test.go`
- Create: `control/control_plane_failover_reload_test.go`
- Create: `cmd/run_failover_reload_test.go`
- Modify: `component/outbound/failover_controller.go:385-465`
- Modify: `component/outbound/dialer_group.go:230-270`
- Modify: `control/control_plane.go:1475-1535`
- Modify: `control/control_plane_drain_test.go:590-810`
- Modify: `cmd/reload_manager.go:25-190`
- Modify: `cmd/run.go:560-720,820-880,1020-1045`

**Interfaces:**
- Produces: `FailoverReloadIdentity` containing ordered names, fixed Fallback, rotation attempts, and all four recovery parameters.
- Produces: expanded `FailoverControllerSnapshot` with identity names, rotation state, total failures, timer state, and `ProbeInFlight`.
- Produces: `outbound.FailoverReloadTransfer` with idempotent `Commit()` and `Rollback()`.
- Produces: `DialerGroup.PrepareFailoverReloadFrom(old *DialerGroup) (*FailoverReloadTransfer, string)`; the reason is empty on compatible transfer and deterministic on reset.
- Produces: `control.ReloadInheritance` with `HasOverlap() bool`, `Commit()`, and `Rollback()`.

- [ ] **Step 1: Write snapshot and compatibility tests**

In `component/outbound/failover_reload_test.go`, table-test identical configuration and one-at-a-time changes to:

```go
[]string{
	"fallback",
	"primary candidates",
	"primary_rotation_attempts",
	"recovery_probe_initial",
	"recovery_probe_max",
	"recovery_successes",
	"recovery_stable_time",
}
```

For identical configuration, assert B/current target names, `rotationActive`, `failedRecoveryProbes`, confirmation fields, `currentDelay`, and remaining timer are restored. For each change, assert reset to priority `0` and the expected reason: `fallback_changed`, `primary_candidates_changed`, or `recovery_policy_changed`. Add one all-fields-changed case to prove reason precedence.

- [ ] **Step 2: Write in-flight ownership and rollback tests**

Block a probe on a channel, prepare transfer, and assert:

1. the old timer/probe is paused and its result cannot mutate either generation;
2. the new controller schedules exactly one immediate probe of the same named target;
3. `Commit()` leaves old recovery paused until close;
4. `Rollback()` cancels the new generation and resumes exactly one old-generation probe with the captured state;
5. calling `Commit()` or `Rollback()` twice has no second effect.

- [ ] **Step 3: Run reload unit tests and verify RED**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./component/outbound -run FailoverReload -count=1'
```

Expected: FAIL because snapshots contain neither dynamic identities nor transactional probe ownership.

- [ ] **Step 4: Implement the reload identity and snapshot**

Create `component/outbound/failover_reload.go` with:

```go
type FailoverReloadIdentity struct {
	PrimaryCandidates []string
	Fallback          string
	Recovery          FailoverRecoveryConfig
}

type FailoverControllerSnapshot struct {
	State                failoverState
	CurrentPrimaryName   string
	RecoveryTargetName   string
	RotationActive       bool
	FailedRecoveryProbes int
	RecoverySuccesses    int
	StableSince          time.Time
	CurrentDelay         time.Duration
	NextProbeAt          time.Time
	ProbeInFlight        bool
}
```

Move snapshot/restore methods out of `failover_controller.go`. Match names into the replacement candidate list; do not carry indexes or pointers across generations.

Compare reload identity in this deterministic order so the reset reason is stable:

```go
func compareFailoverReloadIdentity(old, next FailoverReloadIdentity) (bool, string) {
	if old.Fallback != next.Fallback {
		return false, "fallback_changed"
	}
	if !slices.Equal(old.PrimaryCandidates, next.PrimaryCandidates) {
		return false, "primary_candidates_changed"
	}
	if old.Recovery != next.Recovery {
		return false, "recovery_policy_changed"
	}
	return true, ""
}
```

Keep `CaptureSnapshot` and `RestoreSnapshot` as test-facing wrappers, but route production warm reload through `PrepareFailoverReloadFrom` so pausing the old generation and restoring the new generation are one operation.

- [ ] **Step 5: Implement pause/commit/rollback transfer semantics**

`prepareReloadTransfer` must lock the old controller, capture whether a probe is in flight, stop the pending timer, increment generation, cancel the probe, and leave its atomic active selection unchanged. Restore the new controller with remaining delay, or one immediate scheduled probe when `ProbeInFlight` is true.

Use `sync.Once` to make these terminal operations exclusive:

```go
type FailoverReloadTransfer struct {
	once     sync.Once
	old      *FailoverController
	next     *FailoverController
	snapshot FailoverControllerSnapshot
}

func (t *FailoverReloadTransfer) Commit() {
	t.once.Do(func() {})
}

func (t *FailoverReloadTransfer) Rollback() {
	t.once.Do(func() {
		t.next.cancelReloadRestore()
		t.old.resumeAfterReloadAbort(t.snapshot)
	})
}
```

`cancelReloadRestore` invalidates the replacement generation's timer/probe and is idempotent with `Close`. Do not interpret cancellation as a failed recovery result.

- [ ] **Step 6: Aggregate transfers in the control plane**

Change `InheritDialerHealthFrom` to return:

```go
type ReloadInheritance struct {
	hasOverlap bool
	transfers  []*outbound.FailoverReloadTransfer
}

func (r *ReloadInheritance) HasOverlap() bool { return r != nil && r.hasOverlap }
func (r *ReloadInheritance) Commit() {
	for _, transfer := range r.transfers {
		transfer.Commit()
	}
}
func (r *ReloadInheritance) Rollback() {
	for i := len(r.transfers) - 1; i >= 0; i-- {
		r.transfers[i].Rollback()
	}
}
```

Compatible groups create a transfer through `group.PrepareFailoverReloadFrom(oldGroup)`. Incompatible groups remain at the new initial Primary and emit info-level `failover_rotation_state_reset` with deterministic reason and old/new Primary names. Update the three existing `InheritDialerHealthFrom` assertions in `control/control_plane_drain_test.go` to retain the returned transaction, assert `HasOverlap()`, and call `Commit()` during cleanup so the signature change does not leave a paused test controller.

- [ ] **Step 7: Wire transaction ownership through staged handoff**

Define the narrow lifecycle interface in `cmd/reload_manager.go` so command tests can supply a recorder while production receives `*control.ReloadInheritance`:

```go
type reloadInheritanceTxn interface {
	HasOverlap() bool
	Commit()
	Rollback()
}
```

Add `reloadInheritance reloadInheritanceTxn` to `stagedReloadHandoff`. At each `newC.InheritDialerHealthFrom(oldC)` call, store the returned object and use `HasOverlap()` for drain policy.

- For non-staged cutover, call `Commit()` before starting old-control-plane retirement.
- For staged cutover success, call `Commit()` immediately before clearing the pending handoff and retiring the old plane.
- For staged rollback, close the new plane first, call `Rollback()`, then republish/rebuild/restart the old plane.

- [ ] **Step 8: Add control/cmd transaction tests**

In `control/control_plane_failover_reload_test.go`, create two groups and prove both transfers commit or both roll back. In `cmd/run_failover_reload_test.go`, implement a `recordingReloadInheritance` satisfying `reloadInheritanceTxn`; assert successful handoff calls only `Commit`, while `rollbackStagedReloadHandoff` calls only `Rollback` after `newCancel` has fired. The outbound transfer test from Step 2 separately proves `Rollback` invalidates the replacement controller before resuming the old controller.

- [ ] **Step 9: Test fresh restart behavior**

Promote B, close the controller, build a new controller from the same configuration without a snapshot, and assert current Primary A, zero failure count, no rotation, and no file/config write call.

- [ ] **Step 10: Run reload and race tests**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -race -tags dae_stub_ebpf ./component/outbound ./control ./cmd -run "FailoverReload|ReloadInheritance|StagedReload" -count=1'
```

Expected: PASS with one probe owner and no data races.

- [ ] **Step 11: Commit transactional reload support**

```bash
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation add component/outbound/failover_reload.go component/outbound/failover_reload_test.go component/outbound/failover_controller.go component/outbound/dialer_group.go control/control_plane.go control/control_plane_drain_test.go control/control_plane_failover_reload_test.go cmd/reload_manager.go cmd/run.go cmd/run_failover_reload_test.go
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation commit -m "feat(failover): preserve primary rotation across reload"
```

---

### Task 6: Complete integration coverage and user-facing documentation

**Files:**
- Modify: `component/outbound/failover_rotation_integration_test.go`
- Modify: `config/desc.go:90-110`
- Modify: `example.dae:340-440`

**Interfaces:**
- Consumes: completed config, rotation controller, events, and reload transfer.
- Produces: one real-Dialer A/Fallback/B/C integration harness and documented configuration semantics.

- [ ] **Step 1: Add the complete A-to-B recovery integration scenario**

Use real local HTTP health servers and named direct dialers. Exercise:

```text
A TCP unavailable
-> fixed Fallback selected
-> five failed A probes
-> one failed B probe
-> C recovery starts but fails confirmation
-> A fails after circular wrap
-> B gets three successes across stable time
-> B becomes current Primary
-> A healthy callback does not preempt B
-> B future failure immediately selects the same fixed Fallback
```

Assert new TCP and UDP selections at each active-role transition and leave existing test connections untouched.

- [ ] **Step 2: Add all-unavailable and bounded-exclusion integration scenarios**

Run more than two complete B/C/A wraps with all Primary candidates unavailable. Assert Fallback stays active, one timer/probe maximum, and excluding the Fallback returns `ErrNoAliveDialer` without selecting an unconfirmed candidate.

- [ ] **Step 3: Run integration tests**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -race -tags dae_stub_ebpf ./component/outbound -run "FailoverRotation.*Integration|FailoverRotation.*Unavailable" -count=1'
```

Expected: PASS.

- [ ] **Step 4: Document the configuration in `example.dae`**

Replace the two-node-only failover comments with an opt-in example that states:

```dae
    # proxy_failover {
    #     filter: name(node_A) [priority: 0] # initial Primary after process start
    #     filter: name(xray_local) [priority: 1] # fixed Fallback
    #     filter: name(node_B) [priority: 2] # first standby Primary
    #     filter: name(node_C) [priority: 3] # second standby Primary
    #     policy: failover
    #     primary_rotation_attempts: 5
    #     recovery_probe_initial: 15s
    #     recovery_probe_max: 5m
    #     recovery_successes: 3
    #     recovery_stable_time: 30s
    # }
```

Document that warm reload preserves compatible in-memory state, restart resets to priority `0`, priority gaps are allowed, and no candidate scanning occurs while healthy.

- [ ] **Step 5: Update generated configuration descriptions**

Add `failover` to the `policy` description in `config/desc.go` and add a `primary_rotation_attempts` description that says zero disables rotation and positive values count failed recovery probes before circular standby scanning.

- [ ] **Step 6: Run formatting and focused regression tests**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && gofmt -w config/config.go config/desc.go config/failover_rotation_test.go cmd/validate.go cmd/validate_failover_rotation_test.go cmd/reload_manager.go cmd/run.go cmd/run_failover_reload_test.go component/outbound/dialer_selection_policy.go component/outbound/dialer_selection_policy_test.go component/outbound/dialer_group.go component/outbound/failover_controller.go component/outbound/failover_event.go component/outbound/failover_reload.go component/outbound/failover_controller_test.go component/outbound/failover_rotation_test.go component/outbound/failover_rotation_integration_test.go component/outbound/failover_reload_test.go control/control_plane.go control/control_plane_drain_test.go control/control_plane_failover_reload_test.go && go test -tags dae_stub_ebpf ./component/outbound ./config ./control ./cmd -count=1'
```

Expected: PASS.

- [ ] **Step 7: Commit integration coverage and docs**

```bash
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation add component/outbound/failover_rotation_integration_test.go config/desc.go example.dae
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation commit -m "docs(failover): explain primary rotation recovery"
```

---

### Task 7: Run full Linux verification and prepare the source branch

**Files:**
- Verify: all files changed in Tasks 1-6
- Verify: `docs/superpowers/specs/2026-07-21-failover-primary-rotation-design.md`

**Interfaces:**
- Consumes: the complete source implementation.
- Produces: a clean, rebased feature branch ready for merge into `gateway/main`.

- [ ] **Step 1: Run focused Linux tests**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./component/outbound ./config ./control ./cmd -count=1'
```

Expected: PASS.

- [ ] **Step 2: Run the complete stub-eBPF suite**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -tags dae_stub_ebpf ./... -count=1'
```

Expected: PASS for every package.

- [ ] **Step 3: Run race coverage for the affected packages**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation && go test -race -tags dae_stub_ebpf ./component/outbound ./control ./cmd -count=1'
```

Expected: PASS with no race report.

- [ ] **Step 4: Run source hygiene checks**

```bash
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation diff --check gateway/main...HEAD
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation status --short
```

Expected: no whitespace errors and no uncommitted files.

- [ ] **Step 5: Rebase onto the latest local integration branch**

```bash
git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation rebase gateway/main
```

Expected: successful rebase with the functional commits preserved.

- [ ] **Step 6: Repeat focused tests after rebase**

Run the Task 7 Step 1 command again. Expected: PASS.

- [ ] **Step 7: Request code review against the committed design**

Invoke `$requesting-code-review` with the validated extract at `/tmp/failover-primary-rotation-implementation-spec.md`, the design path `docs/superpowers/specs/2026-07-21-failover-primary-rotation-design.md`, and the exact diff command `git -C /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-primary-rotation diff gateway/main...HEAD`. Require per-criterion evidence and fix any blocking findings in functionality-scoped commits before merge.

---

### Task 8: Merge, build amd64, and perform the gated production rollout

**Files:**
- Modify live only after approval: `/etc/dae/config.dae` on `vm-ubuntu-agent`
- Update after successful rollout: `/Users/lihu/git/dae-config/docs/network/dae-current-state.md`
- Update after successful rollout: `/Users/lihu/git/dae-config/docs/network/dae-operation-log.md`

**Interfaces:**
- Consumes: reviewed feature branch, approved exact priority-to-node mapping, existing fixed `xray_local` Fallback, and `$dae-gateway-operations` production workflow.
- Produces: merged `gateway/main`, validated Linux amd64 binary, enabled live rotation setting, rollback evidence, and updated operational truth.

- [ ] **Step 1: Stop for the exact candidate mapping approval**

Read the live `proxy_failover` group and available live dialer names without printing subscription URLs or credentials. Present this exact mapping for user approval before any write:

```text
priority 0 = existing live Primary
priority 1 = xray_local
priority 2 = first approved standby name
priority 3 = second approved standby name
```

Do not infer or auto-select priorities `2+`. If the user has not approved the exact names and order, stop the production portion while leaving source work complete.

- [ ] **Step 2: Fast-forward the reviewed feature branch into `gateway/main`**

```bash
git -C /Users/lihu/git/dae-config/dae switch gateway/main
git -C /Users/lihu/git/dae-config/dae merge --ff-only feat/failover-primary-rotation
```

Expected: `gateway/main` advances to the reviewed implementation without a merge commit.

- [ ] **Step 3: Build the production Linux amd64 binary in OrbStack**

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae && GOARCH=amd64 make OUTPUT=/tmp/dae-primary-rotation-amd64'
```

Expected: successful `linux/amd64` binary. Verify:

```bash
orbctl run -m ubuntu-24.04 bash -lc 'file /tmp/dae-primary-rotation-amd64 && sha256sum /tmp/dae-primary-rotation-amd64 && /tmp/dae-primary-rotation-amd64 --version'
```

- [ ] **Step 4: Inspect, back up, and edit the live config minimally**

Using one quoted remote script, create a backup named with the remote host's current `YYYYmmdd-HHMMSS` timestamp under `/etc/dae/Backup/`, retain the existing priority `0` Primary and priority `1` `xray_local`, add only the approved exact candidates as priorities `2+`, and add:

```dae
primary_rotation_attempts: 5
```

Never copy the sanitized repository `config.dae` over production.

- [ ] **Step 5: Validate before binary/config activation**

Upload the candidate binary to a temporary gateway path, verify its architecture/hash/version, and run it against the edited live config:

```bash
sudo /path/to/candidate/dae validate -c /etc/dae/config.dae
```

Expected: validation succeeds. On failure, restore the exact backup and stop.

- [ ] **Step 6: Replace the binary atomically with rollback protection**

Back up `/usr/bin/dae`, install the validated candidate atomically, reload/restart according to the standard gateway workflow, and automatically restore both binary and config backups if the service does not become active.

- [ ] **Step 7: Verify the live gateway without manufacturing an outage**

Run:

```bash
/Users/lihu/git/dae-config/skills/dae-gateway-operations/scripts/dae-ops status
/Users/lihu/git/dae-config/skills/dae-gateway-operations/scripts/dae-ops dns
/Users/lihu/git/dae-config/skills/dae-gateway-operations/scripts/dae-ops routing
/Users/lihu/git/dae-config/skills/dae-gateway-operations/scripts/dae-ops failover-bpf
/Users/lihu/git/dae-config/skills/dae-gateway-operations/scripts/dae-ops logs 10
```

Expected: DAE active, TCP/UDP 53 owned by DAE, affected DNS answers valid, routing consistent, every expected failover BPF slot `1`, and no new failover-controller error. If activation or any health check fails, restore and reverify the old config/binary; report rollout incomplete even when rollback is healthy.

- [ ] **Step 8: Record operational truth in the meta-repository**

Update current state and operation log with the deployed version/hash, approved candidate order, backup paths, validation/test evidence, and whether a real rotation was observed. Do not claim end-to-end production rotation unless it occurred naturally or the user separately authorized a controlled outage.

- [ ] **Step 9: Commit operations documentation separately**

```bash
git -C /Users/lihu/git/dae-config add docs/network/dae-current-state.md docs/network/dae-operation-log.md
git -C /Users/lihu/git/dae-config commit -m "docs(network): record failover primary rotation rollout"
```

Expected: DAE source commits remain in the source repository; operational evidence remains in the operations repository.
