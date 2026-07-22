package outbound

import (
	"testing"
	"time"

	"github.com/daeuniverse/dae/component/outbound/dialer"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

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
	poolOrders := [][]string{
		{"A", "B", "C", "X"},
		{"X", "B", "A", "C"},
	}
	for _, poolOrder := range poolOrders {
		poolOrder := poolOrder
		t.Run("pool_"+poolOrder[0]+poolOrder[1]+poolOrder[2]+poolOrder[3], func(t *testing.T) {
			option := testFailoverDialerOption()
			byName := map[string]*dialer.Dialer{
				"A": newNamedDirectDialer(option, "A"),
				"B": newNamedDirectDialer(option, "B"),
				"C": newNamedDirectDialer(option, "C"),
				"X": newNamedDirectDialer(option, "X"),
			}
			pool := make([]*dialer.Dialer, 0, len(poolOrder))
			for _, name := range poolOrder {
				pool = append(pool, byName[name])
			}
			set := &DialerSet{dialers: pool}

			resolved, annotations, cfg, err := set.ResolveFailoverRoles(
				exactNameFunction("C", "A", "B"),
				exactNameFunction("X"),
				FailoverRecoveryConfig{
					ProbeInitial:     15 * time.Second,
					ProbeMax:         5 * time.Minute,
					Successes:        3,
					StableTime:       30 * time.Second,
					RotationAttempts: 5,
				},
			)
			require.NoError(t, err)
			require.Equal(t, []string{"C", "A", "B", "X"}, dialerNamesForRoleTest(resolved))
			require.Len(t, annotations, 4)
			require.Equal(t, []int{0, 1, 2}, cfg.PrimaryCandidateIdxs)
			require.Equal(t, 3, cfg.FallbackIdx)

			controller := NewFailoverControllerWithCandidates(
				log,
				"role-order",
				resolved[:cfg.FallbackIdx],
				resolved[cfg.FallbackIdx],
				cfg.Recovery,
			)
			defer controller.Close()
			active, usingFallback := controller.ActiveDialer()
			require.Same(t, byName["C"], active)
			require.False(t, usingFallback)
		})
	}
}

func TestResolveFailoverRolesCardinality(t *testing.T) {
	option := testFailoverDialerOption()
	a := newNamedDirectDialer(option, "A")
	b := newNamedDirectDialer(option, "B")
	c := newNamedDirectDialer(option, "C")
	x := newNamedDirectDialer(option, "X")
	set := &DialerSet{dialers: []*dialer.Dialer{a, b, c, x}}

	validRecovery := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}

	tests := []struct {
		name     string
		primary  *config_parser.Function
		fallback *config_parser.Function
		attempts int
	}{
		{"one primary attempts 0", exactNameFunction("A"), exactNameFunction("X"), 0},
		{"one primary attempts 5", exactNameFunction("A"), exactNameFunction("X"), 5},
		{"three primaries attempts 0", exactNameFunction("A", "B", "C"), exactNameFunction("X"), 0},
		{"three primaries attempts 5", exactNameFunction("A", "B", "C"), exactNameFunction("X"), 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recovery := validRecovery
			recovery.RotationAttempts = tt.attempts
			_, _, _, err := set.ResolveFailoverRoles(tt.primary, tt.fallback, recovery)
			require.NoError(t, err)
		})
	}

	invalid := []struct {
		name      string
		primary   *config_parser.Function
		fallback  *config_parser.Function
		attempts  int
		wantError string
	}{
		{"empty primary", exactNameFunction(), exactNameFunction("X"), 5, "at least one parameter"},
		{"empty fallback", exactNameFunction("A"), exactNameFunction(), 5, "at least one parameter"},
		{"multiple fallback", exactNameFunction("A"), exactNameFunction("X", "B"), 5, "exactly one name"},
		{"negative attempts", exactNameFunction("A"), exactNameFunction("X"), -1, "must not be negative"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			recovery := validRecovery
			recovery.RotationAttempts = tt.attempts
			_, _, _, err := set.ResolveFailoverRoles(tt.primary, tt.fallback, recovery)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestResolveFailoverRolesExactnessAndUniqueness(t *testing.T) {
	option := testFailoverDialerOption()
	a := newNamedDirectDialer(option, "A")
	b := newNamedDirectDialer(option, "B")
	c := newNamedDirectDialer(option, "C")
	x := newNamedDirectDialer(option, "X")
	set := &DialerSet{dialers: []*dialer.Dialer{a, b, c, x}}

	validRecovery := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}

	tests := []struct {
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
		{"duplicate primary", exactNameFunction("A", "A"), exactNameFunction("X"), "same dialer"},
		{"wrong fallback function", exactNameFunction("A"), &config_parser.Function{Name: "subtag", Params: []*config_parser.Param{{Val: "X"}}}, "fallback"},
		{"negated fallback", exactNameFunction("A"), &config_parser.Function{Name: "name", Not: true, Params: []*config_parser.Param{{Val: "X"}}}, "negated"},
		{"keyed fallback", exactNameFunction("A"), &config_parser.Function{Name: "name", Params: []*config_parser.Param{{Key: "regex", Val: "X"}}}, "regex"},
		{"empty fallback name", exactNameFunction("A"), exactNameFunction(""), "empty"},
		{"unknown fallback", exactNameFunction("A"), exactNameFunction("missing"), "missing"},
		{"fallback overlaps primary", exactNameFunction("A", "B"), exactNameFunction("B"), "overlaps primary"},
		{"empty primary function", &config_parser.Function{Name: "name"}, exactNameFunction("X"), "at least one parameter"},
		{"multiple fallback names", exactNameFunction("A"), exactNameFunction("X", "Y"), "exactly one name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := set.ResolveFailoverRoles(tt.primary, tt.fallback, validRecovery)
			require.ErrorContains(t, err, tt.wantError)
		})
	}

	t.Run("ambiguous name", func(t *testing.T) {
		a1 := newNamedDirectDialer(option, "A")
		a2 := newNamedDirectDialer(option, "A")
		x1 := newNamedDirectDialer(option, "X")
		set2 := &DialerSet{dialers: []*dialer.Dialer{a1, a2, x1}}
		_, _, _, err := set2.ResolveFailoverRoles(exactNameFunction("A"), exactNameFunction("X"), validRecovery)
		require.ErrorContains(t, err, "matches 2 dialers")
	})

	t.Run("ambiguous fallback name", func(t *testing.T) {
		a1 := newNamedDirectDialer(option, "A")
		x1 := newNamedDirectDialer(option, "X")
		x2 := newNamedDirectDialer(option, "X")
		set2 := &DialerSet{dialers: []*dialer.Dialer{a1, x1, x2}}
		_, _, _, err := set2.ResolveFailoverRoles(exactNameFunction("A"), exactNameFunction("X"), validRecovery)
		require.ErrorContains(t, err, "fallback dialer \"X\" matches 2 dialers")
	})
}
