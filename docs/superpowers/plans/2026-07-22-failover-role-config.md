# Failover Role Configuration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace priority-annotated failover filters with ordered `primary: name(...)` and fixed `fallback: name(...)` roles, using one state machine for any Primary count and per-candidate consecutive recovery-failure budgets.

**Architecture:** Add policy-specific role values to `config.Group`, resolve them through a dedicated exact-name `DialerSet` path that preserves parameter order, and feed the resulting ordered dialer slice into the existing `FailoverConfig`/`FailoverController`. Keep reload inheritance transactional and all-or-nothing: identical role/policy identity inherits the full snapshot, while any mismatch starts the replacement controller at its own first Primary without rejecting reload.

**Tech Stack:** Go 1.26+, DAE config parser, `component/outbound` failover controller, table-driven Go tests, OrbStack Ubuntu 24.04 Linux test environment, Git worktrees.

## Global Constraints

- `policy: failover` remains mandatory and must never be inferred from role fields.
- Failover `primary` and `fallback` accept one non-negated exact `name(...)` function only; keyword, regex, subtag, negation, chained functions, string-only values, and empty names are invalid.
- `primary` contains at least one unique dialer in parameter order; `fallback` contains exactly one dialer distinct from every Primary.
- The old `filter: ... [priority: N]` failover format is intentionally unsupported and must not be translated.
- `primary_rotation_attempts` accepts every non-negative integer with one or many Primaries; zero disables index advancement.
- A candidate advances only after its own consecutive failure count reaches the positive threshold; advancement and any successful probe reset that count.
- Current Primary, recovery target, counters, timers, and probe ownership remain memory-only; a process restart starts from `primary[0]`.
- A Primary failure always selects the fixed Fallback first; no unconfirmed Primary candidate carries production traffic.
- Reload inheritance is exact-match or full reset; topology/policy mismatch does not fail reload and must not mutate the old controller before cutover.
- Keep generic non-failover filters and `add_latency` behavior unchanged.
- Run Linux-specific tests in OrbStack with `GOCACHE=/tmp/dae-go-cache`; do not treat macOS Linux-syscall build failures as product failures.
- Preserve unrelated user changes and keep DAE source commits inside `/Users/lihu/git/dae-config/dae/.worktrees/feat/failover-role-config`.

---

### Task 1: Decode Explicit Failover Role Fields

**Files:**
- Modify: `config/config.go:64-167`
- Modify: `config/function_union_test.go`
- Modify: `config/failover_rotation_test.go`

**Interfaces:**
- Consumes: existing `FunctionOrString` and `ParseFunctionOrString` parser union.
- Produces: `config.Group.Primary FunctionOrString` and `config.Group.Fallback FunctionOrString`; both are `nil` when absent and contain one parsed `name(...)` function when present.

- [ ] **Step 1: Write failing decode tests for ordered roles**

Replace the priority-based decoder fixture in `config/failover_rotation_test.go` and add assertions that inspect the parsed function parameters:

```go
func decodeRotationGroup(t *testing.T, setting string) Group {
	t.Helper()
	raw := `
global {}
routing { fallback: direct }
group {
  proxy_failover {
    primary: name(A, B, C)
    fallback: name(xray_local)
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

func TestGroupFailoverRolesDecodeNameFunctions(t *testing.T) {
	g := decodeRotationGroup(t, "primary_rotation_attempts: 5")
	primary, err := ParseFunctionOrString(g.Primary)
	require.NoError(t, err)
	fallback, err := ParseFunctionOrString(g.Fallback)
	require.NoError(t, err)
	require.Equal(t, "name", primary.Name)
	require.Equal(t, []string{"A", "B", "C"}, []string{
		primary.Params[0].Val,
		primary.Params[1].Val,
		primary.Params[2].Val,
	})
	require.Equal(t, "name", fallback.Name)
	require.Equal(t, "xray_local", fallback.Params[0].Val)
}
```

- [ ] **Step 2: Run the focused config tests and verify RED**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./config -run 'TestGroup(FailoverRoles|PrimaryRotation)' -count=1
```

Expected: FAIL because `config.Group` has no `Primary` or `Fallback` fields.

- [ ] **Step 3: Add the conditional role fields without making them globally required**

Add these fields next to `Filter` and `Policy` in `config.Group`:

```go
Primary  FunctionOrString `mapstructure:"primary"`
Fallback FunctionOrString `mapstructure:"fallback"`
```

Do not add `required:""`; failover-only presence is validated after policy parsing.

- [ ] **Step 4: Make the single-function union error field-neutral**

In `ParseFunctionOrString`, replace the hard-coded `fallback` wording so both role fields can wrap it accurately:

```go
case []*config_parser.Function:
	if len(fs) == 1 {
		return fs[0], nil
	}
	return nil, fmt.Errorf("expected exactly 1 function, got %d", len(fs))
```

Add a unit test in `config/function_union_test.go` asserting the exact neutral error for two functions.

- [ ] **Step 5: Run config tests and verify GREEN**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./config -count=1
```

Expected: PASS for `github.com/daeuniverse/dae/config`.

- [ ] **Step 6: Commit the schema change**

```bash
git add config/config.go config/function_union_test.go config/failover_rotation_test.go
git commit -m "feat(config): add explicit failover role fields"
```

---

### Task 2: Resolve Ordered Exact-Name Roles

**Files:**
- Create: `component/outbound/failover_roles.go`
- Create: `component/outbound/failover_roles_test.go`
- Modify: `component/outbound/dialer_group.go:24-180`

**Interfaces:**
- Consumes: `*config_parser.Function` for Primary and Fallback, `DialerSet.dialers`, and `FailoverRecoveryConfig`.
- Produces: `(*DialerSet).ResolveFailoverRoles(primary, fallback *config_parser.Function, recovery FailoverRecoveryConfig) (dialers []*dialer.Dialer, annotations []*dialer.Annotation, cfg *FailoverConfig, err error)`.
- Produces: ordered `dialers` as `[primary[0], primary[1], ..., fallback]`; `cfg.PrimaryCandidateIdxs` covers the ordered prefix and `cfg.FallbackIdx` names the final element.

- [ ] **Step 1: Write failing resolver order and cardinality tests**

Create `component/outbound/failover_roles_test.go` in package `outbound`. Use existing named direct-dialer test helpers and construct `DialerSet` directly so pool order is controlled:

```go
func exactNameFunction(values ...string) *config_parser.Function {
	params := make([]*config_parser.Param, 0, len(values))
	for _, value := range values {
		params = append(params, &config_parser.Param{Val: value})
	}
	return &config_parser.Function{Name: "name", Params: params}
}

func dialerNamesForRoleTest(dialers []*dialer.Dialer) []string {
	names := make([]string, 0, len(dialers))
	for _, d := range dialers {
		names = append(names, d.Property().Name)
	}
	return names
}

func TestResolveFailoverRolesPreservesPrimaryParameterOrder(t *testing.T) {
	option := testFailoverDialerOption()
	a := newNamedDirectDialer(option, "A")
	b := newNamedDirectDialer(option, "B")
	c := newNamedDirectDialer(option, "C")
	x := newNamedDirectDialer(option, "X")
	set := &DialerSet{dialers: []*dialer.Dialer{a, b, c, x}}

	resolved, annotations, cfg, err := set.ResolveFailoverRoles(
		exactNameFunction("C", "A", "B"),
		exactNameFunction("X"),
		FailoverRecoveryConfig{
			ProbeInitial: 15 * time.Second,
			ProbeMax: 5 * time.Minute,
			Successes: 3,
			StableTime: 30 * time.Second,
			RotationAttempts: 5,
		},
	)
	require.NoError(t, err)
	require.Equal(t, []string{"C", "A", "B", "X"}, dialerNamesForRoleTest(resolved))
	require.Len(t, annotations, 4)
	require.Equal(t, []int{0, 1, 2}, cfg.PrimaryCandidateIdxs)
	require.Equal(t, 3, cfg.FallbackIdx)
}
```

Add table cases for one Primary with attempts `0` and `5`, three Primaries with attempts `0` and `5`, empty Primary, and multiple Fallback names.

- [ ] **Step 2: Write failing exactness and uniqueness tests**

Add table cases whose function/name setup covers:

```go
[]struct {
	name      string
	primary   *config_parser.Function
	fallback  *config_parser.Function
	wantError string
}{
	{"wrong primary function", &config_parser.Function{Name: "subtag", Params: []*config_parser.Param{{Val: "A"}}}, exactNameFunction("X"), "primary"},
	{"negated primary", &config_parser.Function{Name: "name", Not: true, Params: []*config_parser.Param{{Val: "A"}}}, exactNameFunction("X"), "negated"},
	{"keyed primary", &config_parser.Function{Name: "name", Params: []*config_parser.Param{{Key: "keyword", Val: "A"}}}, exactNameFunction("X"), "keyword"},
	{"empty primary name", exactNameFunction(""), exactNameFunction("X"), "empty"},
	{"unknown primary", exactNameFunction("missing"), exactNameFunction("X"), "missing"},
	{"duplicate primary", exactNameFunction("A", "A"), exactNameFunction("X"), "duplicate"},
	{"fallback overlaps primary", exactNameFunction("A", "B"), exactNameFunction("B"), "fallback"},
}
```

Add a separate case with two pool dialers both named `A` and require an ambiguity error containing `A` and the match count.

- [ ] **Step 3: Run resolver tests and verify RED**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./component/outbound -run 'TestResolveFailoverRoles' -count=1
```

Expected: FAIL because `ResolveFailoverRoles` does not exist.

- [ ] **Step 4: Implement the dedicated resolver**

Create `component/outbound/failover_roles.go` with these responsibilities and signatures:

```go
func exactFailoverRoleNames(role string, fn *config_parser.Function, requireOne bool) ([]string, error)

func (s *DialerSet) resolveExactDialer(role, name string) (*dialer.Dialer, error)

func (s *DialerSet) ResolveFailoverRoles(
	primary *config_parser.Function,
	fallback *config_parser.Function,
	recovery FailoverRecoveryConfig,
) (
	dialers []*dialer.Dialer,
	annotations []*dialer.Annotation,
	cfg *FailoverConfig,
	err error,
)
```

The core resolution loop must iterate requested names first, never pool order:

```go
ordered := make([]*dialer.Dialer, 0, len(primaryNames)+1)
seen := make(map[*dialer.Dialer]string, len(primaryNames)+1)
for _, name := range primaryNames {
	d, err := s.resolveExactDialer("primary", name)
	if err != nil {
		return nil, nil, nil, err
	}
	if previous, ok := seen[d]; ok {
		return nil, nil, nil, fmt.Errorf("primary name %q resolves to the same dialer as %q", name, previous)
	}
	seen[d] = name
	ordered = append(ordered, d)
}
fallbackDialer, err := s.resolveExactDialer("fallback", fallbackNames[0])
if err != nil {
	return nil, nil, nil, err
}
if primaryName, ok := seen[fallbackDialer]; ok {
	return nil, nil, nil, fmt.Errorf("fallback name %q overlaps primary %q", fallbackNames[0], primaryName)
}
ordered = append(ordered, fallbackDialer)
```

Build indexes without a candidate-count branch:

```go
primaryIdxs := make([]int, len(primaryNames))
for i := range primaryIdxs {
	primaryIdxs[i] = i
}
annotations = make([]*dialer.Annotation, len(ordered))
for i := range annotations {
	annotations[i] = &dialer.Annotation{}
}
cfg = &FailoverConfig{
	PrimaryCandidateIdxs: primaryIdxs,
	FallbackIdx: len(primaryNames),
	Recovery: recovery,
}
```

Extract the current recovery duration/success/negative-attempt checks from `ValidateFailoverGroup` into `validateFailoverRecoveryConfig` and call it before name resolution.

- [ ] **Step 5: Run resolver and existing outbound tests**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./component/outbound -run 'TestResolveFailoverRoles|TestValidateFailoverGroup' -count=1
```

Expected: PASS; the old validator remains temporarily available until Task 3 switches every caller.

- [ ] **Step 6: Commit the ordered resolver**

```bash
git add component/outbound/failover_roles.go component/outbound/failover_roles_test.go component/outbound/dialer_group.go
git commit -m "feat(outbound): resolve ordered failover roles"
```

---

### Task 3: Wire Role Resolution and Remove Priority Failover

**Files:**
- Create: `control/group_dialer_resolver.go`
- Create: `control/group_dialer_resolver_test.go`
- Modify: `control/control_plane.go:760-835`
- Modify: `component/outbound/dialer_group.go:24-180`
- Modify: `component/outbound/dialer/annotation.go`
- Modify: `component/outbound/failover_controller_test.go`
- Modify: `component/outbound/failover_rotation_integration_test.go`
- Modify: `component/outbound/dialer_group_test.go`
- Modify: `component/outbound/failover_integration_test.go`
- Modify: `control/control_plane_failover_reload_test.go`
- Modify: `config/failover_notify_test.go`
- Modify: `config/failover_notify_validate_test.go`
- Modify: `cmd/validate_failover_rotation_test.go`
- Modify: `cmd/validate_failover_notify_test.go`

**Interfaces:**
- Consumes: Task 1 role fields and Task 2 `ResolveFailoverRoles`.
- Produces: `resolveConfiguredGroupDialers(dialerSet *outbound.DialerSet, group config.Group, policy outbound.DialerSelectionPolicy) ([]*dialer.Dialer, []*dialer.Annotation, *outbound.FailoverConfig, error)`.
- Removes: priority parsing from `dialer.Annotation` and the annotation-based `ValidateFailoverGroup` entry point.

- [ ] **Step 1: Write policy-boundary tests before wiring**

Create `control/group_dialer_resolver_test.go` with invalid cases that fail before dialer resolution:

```go
func exactControlNameFunction(value string) *config_parser.Function {
	return &config_parser.Function{
		Name: "name",
		Params: []*config_parser.Param{{Val: value}},
	}
}

func TestResolveConfiguredGroupDialersRejectsPolicyRoleMisuse(t *testing.T) {
	tests := []struct {
		name string
		group config.Group
		policy outbound.DialerSelectionPolicy
		want string
	}{
		{
			name: "failover filter",
			group: config.Group{Name: "g", Filter: [][]*config_parser.Function{{{Name: "name"}}}},
			policy: outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
			want: "group \"g\"",
		},
		{
			name: "non-failover primary",
			group: config.Group{Name: "g", Primary: []*config_parser.Function{{Name: "name"}}},
			policy: outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Random},
			want: "primary",
		},
		{
			name: "missing failover fallback",
			group: config.Group{Name: "g", Primary: []*config_parser.Function{exactControlNameFunction("A")}},
			policy: outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
			want: "fallback",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := resolveConfiguredGroupDialers(nil, tt.group, tt.policy)
			require.ErrorContains(t, err, tt.want)
		})
	}
}
```

Also add parser/build fixtures using the old priority form and require a failover-filter error instead of silent translation.

- [ ] **Step 2: Run the boundary tests and verify RED**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./control -run 'TestResolveConfiguredGroupDialers' -count=1
```

Expected: FAIL because the helper does not exist.

- [ ] **Step 3: Implement policy-specific group resolution**

Create `control/group_dialer_resolver.go`:

```go
func resolveConfiguredGroupDialers(
	dialerSet *outbound.DialerSet,
	group config.Group,
	policy outbound.DialerSelectionPolicy,
) ([]*dialer.Dialer, []*dialer.Annotation, *outbound.FailoverConfig, error) {
	if policy.Policy == consts.DialerSelectionPolicy_Failover {
		if len(group.Filter) != 0 {
			return nil, nil, nil, fmt.Errorf("group %q: policy failover does not allow filter; use primary and fallback", group.Name)
		}
		primary, err := config.ParseFunctionOrString(group.Primary)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("group %q primary: %w", group.Name, err)
		}
		fallback, err := config.ParseFunctionOrString(group.Fallback)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("group %q fallback: %w", group.Name, err)
		}
		recovery := outbound.FailoverRecoveryConfig{
			ProbeInitial: group.RecoveryProbeInitial,
			ProbeMax: group.RecoveryProbeMax,
			Successes: group.RecoverySuccesses,
			StableTime: group.RecoveryStableTime,
			RotationAttempts: group.PrimaryRotationAttempts,
		}
		dialers, annotations, cfg, err := dialerSet.ResolveFailoverRoles(primary, fallback, recovery)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("group %q: %w", group.Name, err)
		}
		return dialers, annotations, cfg, nil
	}

	if group.Primary != nil || group.Fallback != nil {
		return nil, nil, nil, fmt.Errorf("group %q: primary and fallback require policy: failover", group.Name)
	}
	dialers, annotations, err := dialerSet.FilterAndAnnotate(group.Filter, group.FilterAnnotation)
	return dialers, annotations, nil, err
}
```

Replace the unconditional `FilterAndAnnotate` plus later failover validation in `control_plane.go` with one call to this helper. Keep option cloning after resolution so the ordered slice and role indexes stay aligned.

- [ ] **Step 4: Remove the priority annotation and old validator**

In `component/outbound/dialer/annotation.go`, retain only `add_latency`:

```go
const AnnotationKey_AddLatency = "add_latency"

type Annotation struct {
	AddLatency time.Duration
}
```

Remove `AnnotationKey_Priority`, `PriorityNotSet`, `Annotation.Priority`, its integer parser, and `ValidateFailoverGroup`. Update `FailoverConfig` comments to describe ordered role indexes rather than numeric priorities.

- [ ] **Step 5: Migrate test construction to ordered dialers**

For package-level controller tests that do not exercise resolution, construct role order directly:

```go
dialers := []*dialer.Dialer{primaryA, primaryB, primaryC, fallback}
annotations := []*dialer.Annotation{{}, {}, {}, {}}
cfg := &FailoverConfig{
	PrimaryCandidateIdxs: []int{0, 1, 2},
	FallbackIdx: 3,
	Recovery: recovery,
}
```

For validation tests, replace old priority cases with `ResolveFailoverRoles` syntax, exactness, ambiguity, duplicate, overlap, recovery-duration, success-count, and negative-attempt cases. Update every textual config fixture to:

```dae
primary: name(node1)
fallback: name(node2)
policy: failover
```

Run this audit and require no production/test caller of the old path:

```bash
rg -n 'ValidateFailoverGroup|PriorityNotSet|AnnotationKey_Priority' component config control cmd
rg -n '\bPriority\s*:' component/outbound control/control_plane_failover_reload_test.go
```

Expected: no matches outside archived documentation.

- [ ] **Step 6: Run configuration, outbound, control, and command tests**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./config ./component/outbound ./control ./cmd -count=1
```

Expected: PASS for all four packages.

- [ ] **Step 7: Commit the cutover from priority roles**

```bash
git add config component/outbound control cmd
git commit -m "refactor(failover): replace priority annotations with roles"
```

---

### Task 4: Make Rotation Attempts Per-Candidate and Consecutive

**Files:**
- Modify: `component/outbound/failover_controller.go:151-167,559-742`
- Modify: `component/outbound/failover_rotation_test.go`
- Modify: `component/outbound/failover_rotation_integration_test.go`
- Modify: `component/outbound/failover_reload_test.go`

**Interfaces:**
- Consumes: existing `FailoverRecoveryConfig.RotationAttempts`, `recoveryTarget`, `rotationActive`, and recovery confirmation state.
- Produces: `failedRecoveryProbes` as the consecutive failure count for the current recovery target; it resets on target advancement, any successful probe, promotion, and fresh failover episode.

- [ ] **Step 1: Write a failing per-candidate budget test**

Add a deterministic test with threshold `2`:

```go
func TestFailoverRotationUsesPerCandidateConsecutiveBudget(t *testing.T) {
	cfg := FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax: 5 * time.Minute,
		Successes: 3,
		StableTime: 30 * time.Second,
		RotationAttempts: 2,
	}
	fc, sched, candidates, _ := newRotationControllerTest(t, cfg)
	defer fc.Close()
	results := []struct {
		target *dialer.Dialer
		ok bool
	}{
		{target: candidates[0]},
		{target: candidates[0]},
		{target: candidates[1]},
		{target: candidates[1], ok: true},
		{target: candidates[1]},
		{target: candidates[1]},
	}
	next := 0
	fc.probeTargetTCP = func(_ context.Context, target *dialer.Dialer) (bool, error) {
		result := results[next]
		next++
		if target != result.target {
			t.Fatalf("probe target = %s, want %s", target.Property().Name, result.target.Property().Name)
		}
		return result.ok, nil
	}
	fc.triggerPrimaryFailureForTest()
	for range results {
		sched.FireNext(t)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.recoveryTarget != 2 {
		t.Fatalf("recoveryTarget = %d, want 2 (C)", fc.recoveryTarget)
	}
	if fc.failedRecoveryProbes != 0 {
		t.Fatalf("failedRecoveryProbes = %d, want 0 after advancement", fc.failedRecoveryProbes)
	}
}
```

The target history proves A receives two failures, B's success resets its count, and B then requires two new failures before C.

- [ ] **Step 2: Add zero-threshold and single-candidate cases**

Use the same constructor/test driver for:

```go
tests := []struct {
	name string
	candidates []string
	attempts int
	failures int
	wantTarget int
	wantFailures int
}{
	{"one candidate positive", []string{"A"}, 2, 2, 0, 0},
	{"one candidate disabled", []string{"A"}, 0, 6, 0, 6},
	{"three candidates disabled", []string{"A", "B", "C"}, 0, 6, 0, 6},
}
```

For each table row, create named dialers from `tt.candidates`, call
`NewFailoverControllerWithCandidates`, install `fakeFailoverScheduler` and an
all-failure `probeTargetTCP`, fire `tt.failures` scheduled probes, then assert
the target/count fields. Require Fallback to remain the active dialer in every
failed-probe state.

- [ ] **Step 3: Run focused rotation tests and verify RED**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./component/outbound -run 'TestFailoverRotation(UsesPerCandidate|CandidateCount)' -count=1
```

Expected: FAIL because the current controller uses an episode-total counter and advances after every failure once rotation is active.

- [ ] **Step 4: Implement one threshold rule for every candidate count**

In `onProbeSuccessLocked`, insert this assignment immediately before
`fc.recoverySuccesses++`:

```go
fc.failedRecoveryProbes = 0
```

In `onProbeFailureLocked`, replace the episode-total advancement branch with:

```go
fc.failedRecoveryProbes++
failedAttempts := fc.failedRecoveryProbes
fc.recoverySuccesses = 0
fc.stableSince = time.Time{}

advances := fc.config.RotationAttempts > 0 &&
	fc.failedRecoveryProbes >= fc.config.RotationAttempts
rotationStarts := advances && !fc.rotationActive

fc.currentDelay = minDuration(fc.currentDelay*2, fc.config.ProbeMax)
if advances {
	fc.rotationActive = true
	fc.recoveryTarget = (fc.recoveryTarget + 1) % len(fc.primaryCandidates)
	fc.failedRecoveryProbes = 0
}
```

Use `failedAttempts` in structured log fields so an advancement log reports the threshold consumed rather than the post-reset zero. Keep `rotationActive` only for first-advance versus later-advance observability; it must not change whether the cursor advances.

- [ ] **Step 5: Update existing sequences to the new budget**

Change tests that currently expect one B/C failure after initial activation. For a threshold of five, the expected sequence is:

```text
A A A A A B B B B B C C C C C A
```

Update `TestFailoverRotationPromotedBFailureStartsAtC` so five failed B probes move to C, the first failed C probe leaves the target at C with count one, and only five failed C probes wrap to A.

Update `reloadTestControllers` to fire seven failures: five move A to B and two leave a nontrivial consecutive count of two on B. Compatible reload assertions must preserve target B and count two.

- [ ] **Step 6: Run all outbound failover tests**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./component/outbound -run 'TestFailover' -count=1
```

Expected: PASS, including target-aware production probe, fixed-Fallback selection, candidate promotion, and structured rotation log tests.

- [ ] **Step 7: Commit the state-machine correction**

```bash
git add component/outbound/failover_controller.go component/outbound/failover_rotation_test.go component/outbound/failover_rotation_integration_test.go component/outbound/failover_reload_test.go
git commit -m "refactor(failover): use per-candidate rotation attempts"
```

---

### Task 5: Preserve All-or-Nothing Reload Semantics

**Files:**
- Modify: `component/outbound/failover_reload.go`
- Modify: `component/outbound/failover_reload_test.go`
- Modify: `control/control_plane_failover_reload_test.go`
- Modify: `cmd/run_failover_reload_test.go`

**Interfaces:**
- Consumes: ordered candidate names, fixed Fallback name, full `FailoverRecoveryConfig`, and the per-target `failedRecoveryProbes` state from Task 4.
- Produces: unchanged `FailoverReloadIdentity`, `FailoverReloadTransfer`, and `ReloadInheritance` APIs with snapshot comments/tests updated to consecutive-counter semantics.

- [ ] **Step 1: Extend identity mismatch tests for reordered roles**

Add explicit membership and order cases to `TestFailoverReloadSnapshotResetsOnIdentityChange`:

```go
{
	name: "primary_candidates_reordered",
	modifyNew: func(c FailoverRecoveryConfig) FailoverRecoveryConfig { return c },
	newCands: []string{"B", "A", "C"},
	newFallback: "fallback",
	wantReason: "primary_candidates_changed",
	wantNewPrime: "B",
},
{
	name: "primary_candidate_removed",
	modifyNew: func(c FailoverRecoveryConfig) FailoverRecoveryConfig { return c },
	newCands: []string{"A", "C"},
	newFallback: "fallback",
	wantReason: "primary_candidates_changed",
	wantNewPrime: "A",
},
```

Require each mismatch to return a nil transfer, leave the old pending timer/probe untouched, initialize the new counter to zero, and allow the replacement controller to remain usable.

- [ ] **Step 2: Add invalid-build isolation coverage**

Create a recovering old controller, capture its snapshot and scheduler ownership, then call the role resolver with an invalid reload role such as `fallback: name(A)` overlapping Primary. Assert the resolution error occurs without invoking `prepareReloadTransfer` and compare the old snapshot/timer ownership before and after:

```go
recovery := baseReloadRecoveryConfig()
oldFC, _, oldSched, _, _, _ := reloadTestControllers(
	t,
	recovery,
	recovery,
	[]string{"A", "B", "C"},
	[]string{"A", "B", "C"},
	"fallback",
	"fallback",
)
defer oldFC.Close()
option := testFailoverDialerOption()
a := newNamedDirectDialer(option, "A")
b := newNamedDirectDialer(option, "B")
set := &DialerSet{dialers: []*dialer.Dialer{a, b}}
before := oldFC.CaptureSnapshot()
beforePending := oldSched.PendingCount()

_, _, _, err := set.ResolveFailoverRoles(
	exactNameFunction("A", "B"),
	exactNameFunction("A"),
	baseReloadRecoveryConfig(),
)
require.ErrorContains(t, err, "overlaps primary")

after := oldFC.CaptureSnapshot()
require.Equal(t, before, after)
require.Equal(t, beforePending, oldSched.PendingCount())
```

Add a command-layer assertion around the existing staged failure path showing that a failed new-generation build does not call inheritance `Commit` or `Rollback` and retains the old generation reference.

- [ ] **Step 3: Run reload tests before production changes**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./component/outbound ./control ./cmd -run 'Test.*Reload' -count=1
```

Expected before final adjustments: failures only where old snapshot comments/count expectations still describe episode-total behavior or where the new order case is missing.

- [ ] **Step 4: Align snapshot capture/restore semantics**

Keep snapshot transfer code structurally unchanged, but update comments and assertions so `FailedRecoveryProbes` means the consecutive count for the named `RecoveryTargetName`. Confirm these assignments remain symmetric:

```go
FailedRecoveryProbes: fc.failedRecoveryProbes,
```

and:

```go
fc.failedRecoveryProbes = snap.FailedRecoveryProbes
```

Do not normalize counters, remap by indexes, partially inherit changed policy, or turn identity mismatch into a reload error.

- [ ] **Step 5: Re-run reload and race-sensitive ownership tests**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./component/outbound ./control ./cmd -run 'Test.*Reload' -count=1
orb env GOCACHE=/tmp/dae-go-cache go test -race -tags dae_stub_ebpf ./component/outbound -run 'TestFailoverReload' -count=1
```

Expected: PASS; compatible commit/rollback has one logical owner, mismatch leaves the old owner untouched, and late detached probes mutate neither generation.

- [ ] **Step 6: Commit reload regression coverage**

```bash
git add component/outbound/failover_reload.go component/outbound/failover_reload_test.go control/control_plane_failover_reload_test.go cmd/run_failover_reload_test.go
git commit -m "test(failover): lock role reload boundaries"
```

---

### Task 6: Replace Active Priority Documentation and Examples

**Files:**
- Modify: `config/desc.go:96-118`
- Modify: `example.dae:390-475`
- Modify: `README.md:26`
- Move: `docs/superpowers/specs/2026-06-07-priority-failover-policy-design.md` to `docs/superpowers/specs/archive/2026-06-07-priority-failover-policy-design.md`
- Move: `docs/superpowers/specs/priority-failover.tracker.md` to `docs/superpowers/specs/archive/priority-failover.tracker.md`
- Modify: failover config strings under `config/*_test.go` and `cmd/*_test.go` if any priority examples remain after Task 3.

**Interfaces:**
- Consumes: final role syntax, per-candidate threshold semantics, restart behavior, and reload identity behavior.
- Produces: active documentation containing no supported priority-role instructions and examples for one and multiple Primaries.

- [ ] **Step 1: Write documentation search assertions**

Run the active-tree search before edits:

```bash
rg -n 'filter:.*\[priority:|priority: [0-9].*(primary|fallback|standby)' README.md example.dae config docs \
  --glob '!docs/superpowers/specs/archive/**' \
  --glob '!docs/superpowers/plans/archive/**' \
  --glob '!docs/superpowers/specs/2026-07-22-failover-role-config-design.md' \
  --glob '!docs/superpowers/plans/2026-07-22-failover-role-config.md'
```

Expected: matches in README, example, descriptions, old active design, and tracker.

- [ ] **Step 2: Update configuration descriptions**

Add `primary` and `fallback` entries to `GroupDesc` and replace the failover policy/rotation text. The descriptions must state:

```text
primary: Ordered exact node names declared as name(A, B, C). The first name is
the initial Primary after process start; keyword/regex/subtag are unsupported.

fallback: One exact node name declared as name(X). It carries traffic while
the current Primary is unavailable and never participates in rotation.

primary_rotation_attempts: Consecutive failed recovery probes allowed for each
current recovery target before advancing. 0 disables advancement. Candidate
count does not restrict the value.
```

- [ ] **Step 3: Replace example blocks**

Use these active examples in `example.dae`:

```dae
# Single Primary; the same state machine is used and rotation is disabled.
#proxy_failover {
#    primary: name(node_A)
#    fallback: name(xray_local)
#    policy: failover
#    primary_rotation_attempts: 0
#}

# Ordered Primary rotation. Each candidate receives five consecutive failed
# recovery probes before the cursor advances; Fallback carries traffic until
# one candidate completes recovery confirmation.
#proxy_failover_rotating {
#    primary: name(node_A, node_B, node_C)
#    fallback: name(xray_local)
#    policy: failover
#    primary_rotation_attempts: 5
#    recovery_probe_initial: 15s
#    recovery_probe_max: 5m
#    recovery_successes: 3
#    recovery_stable_time: 30s
#}
```

Explain that compatible reload inherits memory state, identity mismatch starts the replacement at its new first Primary, and process restart always starts at the configured first Primary.

- [ ] **Step 4: Update README and archive superseded active records**

Change the README feature bullet to explicit role language:

```markdown
- [x] Support event-driven failover groups with ordered `primary: name(...)`, a fixed `fallback: name(...)`, targeted TCP recovery probes, warm-reload state handoff, and structured `failover_switch` events.
```

Move the old design and tracker into the archive paths with `git mv`; do not rewrite historical content after it is archived.

- [ ] **Step 5: Verify active docs and config parsing**

Run:

```bash
rg -n 'filter:.*\[priority:|priority: [0-9].*(primary|fallback|standby)' README.md example.dae config docs \
  --glob '!docs/superpowers/specs/archive/**' \
  --glob '!docs/superpowers/plans/archive/**' \
  --glob '!docs/superpowers/specs/2026-07-22-failover-role-config-design.md' \
  --glob '!docs/superpowers/plans/2026-07-22-failover-role-config.md'
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./config ./cmd -count=1
git diff --check
```

Expected: the search has no active priority-role matches; tests pass; `git diff --check` prints nothing.

- [ ] **Step 6: Commit documentation migration**

```bash
git add README.md example.dae config docs/superpowers/specs cmd
git commit -m "docs(failover): document explicit primary roles"
```

---

### Task 7: Full Linux Verification, Merge, and Coordinated Gateway Migration

**Files:**
- Verify: all files changed by Tasks 1-6
- Update after successful deployment: `/Users/lihu/git/dae-config/docs/network/dae-current-state.md`
- Update after successful deployment: `/Users/lihu/git/dae-config/docs/network/dae-operation-log.md`
- Live source of truth: `/etc/dae/config.dae` on `vm-ubuntu-agent`
- Live binary: `/usr/bin/dae` on `vm-ubuntu-agent`

**Interfaces:**
- Consumes: completed source branch, current production priority mapping (`c55s1`, `xray_local`, `c55s2`, `c55s3`), and the gateway's existing safe deployment workflow.
- Produces: a verified amd64 candidate binary, merged `gateway/main`, live role configuration `primary: name(c55s1, c55s2, c55s3)` plus `fallback: name(xray_local)`, paired rollback artifacts, and post-deployment operational evidence.

- [ ] **Step 1: Format and run focused Linux tests**

Run:

```bash
git diff --name-only gateway/main -- '*.go' | xargs gofmt -w
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./config ./component/outbound ./control ./cmd -count=1
orb env GOCACHE=/tmp/dae-go-cache go test -race -tags dae_stub_ebpf ./component/outbound -run 'TestFailover' -count=1
```

Expected: all commands exit zero.

- [ ] **Step 2: Run the full Linux suite and source audits**

Run:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./... -count=1
git diff --check
rg -n 'ValidateFailoverGroup|PriorityNotSet|AnnotationKey_Priority' component config control cmd
rg -n '\bPriority\s*:' component/outbound control/control_plane_failover_reload_test.go
rg -n 'filter:.*\[priority:|priority: [0-9].*(primary|fallback|standby)' README.md example.dae config docs \
  --glob '!docs/superpowers/specs/archive/**' \
  --glob '!docs/superpowers/plans/archive/**' \
  --glob '!docs/superpowers/specs/2026-07-22-failover-role-config-design.md' \
  --glob '!docs/superpowers/plans/2026-07-22-failover-role-config.md'
```

Expected: full test PASS, `git diff --check` empty, and both legacy searches empty.

- [ ] **Step 3: Review, rebase, and fast-forward the feature branch**

Run:

```bash
git status --short
git rebase gateway/main
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./config ./component/outbound ./control ./cmd -count=1
git -C /Users/lihu/git/dae-config/dae switch gateway/main
git -C /Users/lihu/git/dae-config/dae merge --ff-only feat/failover-role-config
```

Expected: feature worktree clean before rebase, focused tests pass after rebase, and `gateway/main` fast-forwards without a merge commit.

- [ ] **Step 4: Build and identify the amd64 candidate**

From the merged source checkout, run:

```bash
orbctl run -m ubuntu-24.04 bash -lc 'cd /Users/lihu/git/dae-config/dae && GOARCH=amd64 make OUTPUT=/Users/lihu/git/dae-config/dae/.worktrees/feat/failover-role-config/dae-failover-role-config-amd64'
file /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-role-config/dae-failover-role-config-amd64
shasum -a 256 /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-role-config/dae-failover-role-config-amd64
orbctl run -m ubuntu-24.04 /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-role-config/dae-failover-role-config-amd64 --version
```

Expected: ELF 64-bit x86-64 binary, recorded SHA256, and a version containing the merged commit.

- [ ] **Step 5: Inspect and prepare paired production backups**

Read the live `proxy_failover` block and current binary/config hashes before mutation. Create timestamped backups under `/etc/dae/Backup` for config and beside `/usr/bin/dae` for the binary. Copy both backups to isolated verification paths and require their hashes to match the originals. Do not use the sanitized repository config as input.

The intended targeted live block migration is:

```dae
primary: name(c55s1, c55s2, c55s3)
fallback: name(xray_local)
policy: failover
primary_rotation_attempts: 5
```

Keep all existing recovery and notification fields unchanged.

- [ ] **Step 6: Validate the staged config with the candidate binary**

Create a staged copy of the live config, replace only the four priority filter role lines with the two explicit role lines, then upload the candidate binary:

```bash
scp /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-role-config/dae-failover-role-config-amd64 vm-ubuntu-agent:/tmp/dae-failover-role-config-amd64
```

Run candidate validation on the gateway:

```bash
sudo /tmp/dae-failover-role-config-amd64 validate -c /etc/dae/Backup/config.dae.failover-role-staged
```

Expected: validation succeeds with the candidate. Also run the installed old binary against the staged config and record that it rejects the new syntax, proving binary and config must move together.

- [ ] **Step 7: Install as one restart transaction with automatic paired rollback**

Atomically install the candidate binary and staged config, then restart DAE. If validation, restart, service activity, listeners, or BPF checks fail, restore both recorded backups, validate the restored config with the restored binary, restart, and repeat health verification.

Required success checks:

```bash
sudo /usr/bin/dae --version
sudo /usr/bin/dae validate -c /etc/dae/config.dae
sudo systemctl is-active dae
sudo systemctl status dae --no-pager
sudo ss -lntup
```

Confirm DAE owns TCP/UDP port 53 and failover BPF connectivity state is initialized. This first incompatible-format activation must use restart, not reload.

- [ ] **Step 8: Verify the real client path and record operations state**

Generate one request from a real LAN client to an existing `proxy_failover` allowlist destination. Correlate it with DAE routing/FakeIP observability and verify the active group uses initial Primary `c55s1`; gateway-local curl alone is insufficient.

After success, update the operations repository on its local `main` branch with the new syntax, binary version/hash, backup paths, restart result, and client-path evidence:

```bash
git -C /Users/lihu/git/dae-config add docs/network/dae-current-state.md docs/network/dae-operation-log.md
git -C /Users/lihu/git/dae-config commit -m "docs(network): record failover role migration"
```

- [ ] **Step 9: Archive the completed feature worktree**

After source merge, production verification, and operations-doc commit:

```bash
git -C /Users/lihu/git/dae-config/dae worktree remove /Users/lihu/git/dae-config/dae/.worktrees/feat/failover-role-config
git -C /Users/lihu/git/dae-config/dae branch -d feat/failover-role-config
```

Expected: `gateway/main` contains all source commits, the temporary worktree is removed, and no unmerged feature branch remains.
