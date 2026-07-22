package control

import (
	"testing"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/outbound"
	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

func exactControlNameFunction(values ...string) *config_parser.Function {
	params := make([]*config_parser.Param, 0, len(values))
	for _, value := range values {
		params = append(params, &config_parser.Param{Val: value})
	}
	return &config_parser.Function{Name: "name", Params: params}
}

func withValidFailoverRecovery(group config.Group) config.Group {
	group.RecoveryProbeInitial = 15 * time.Second
	group.RecoveryProbeMax = 5 * time.Minute
	group.RecoverySuccesses = 3
	group.RecoveryStableTime = 30 * time.Second
	return group
}

func TestResolveConfiguredGroupDialersRejectsPolicyRoleMisuse(t *testing.T) {
	tests := []struct {
		name   string
		group  config.Group
		policy outbound.DialerSelectionPolicy
		want   string
	}{
		{
			name:   "failover filter",
			group:  config.Group{Name: "g", Filter: [][]*config_parser.Function{{{Name: "name"}}}},
			policy: outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
			want:   "group \"g\"",
		},
		{
			name:   "non-failover primary",
			group:  config.Group{Name: "g", Primary: []*config_parser.Function{{Name: "name"}}},
			policy: outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Random},
			want:   "primary",
		},
		{
			name:   "non-failover fallback",
			group:  config.Group{Name: "g", Fallback: []*config_parser.Function{{Name: "name"}}},
			policy: outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Random},
			want:   "fallback",
		},
		{
			name: "chained primary functions",
			group: config.Group{
				Name:     "g",
				Primary:  []*config_parser.Function{exactControlNameFunction("A"), exactControlNameFunction("B")},
				Fallback: []*config_parser.Function{exactControlNameFunction("X")},
			},
			policy: outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
			want:   "group \"g\" primary: expected exactly 1 function, got 2",
		},
		{
			name: "chained fallback functions",
			group: config.Group{
				Name:     "g",
				Primary:  []*config_parser.Function{exactControlNameFunction("A")},
				Fallback: []*config_parser.Function{exactControlNameFunction("X"), exactControlNameFunction("Y")},
			},
			policy: outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
			want:   "group \"g\" fallback: expected exactly 1 function, got 2",
		},
		{
			name:   "missing failover fallback",
			group:  config.Group{Name: "g", Primary: []*config_parser.Function{exactControlNameFunction("A")}},
			policy: outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
			want:   "fallback",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := resolveConfiguredGroupDialers(nil, tt.group, tt.policy)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestResolveConfiguredGroupDialersRejectsLegacyPriorityFailoverRoles(t *testing.T) {
	tests := []struct {
		name  string
		group config.Group
	}{
		{
			name: "two-node legacy roles",
			group: config.Group{
				Name: "legacy_two",
				Filter: [][]*config_parser.Function{
					{exactControlNameFunction("A")},
					{exactControlNameFunction("X")},
				},
				FilterAnnotation: [][]*config_parser.Param{
					{{Key: "priority", Val: "0"}},
					{{Key: "priority", Val: "1"}},
				},
			},
		},
		{
			name: "rotating legacy roles",
			group: config.Group{
				Name: "legacy_rotating",
				Filter: [][]*config_parser.Function{
					{exactControlNameFunction("A")},
					{exactControlNameFunction("X")},
					{exactControlNameFunction("B")},
					{exactControlNameFunction("C")},
				},
				FilterAnnotation: [][]*config_parser.Param{
					{{Key: "priority", Val: "0"}},
					{{Key: "priority", Val: "1"}},
					{{Key: "priority", Val: "2"}},
					{{Key: "priority", Val: "3"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := resolveConfiguredGroupDialers(
				nil,
				withValidFailoverRecovery(tt.group),
				outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
			)
			require.ErrorContains(t, err, "group \""+tt.group.Name+"\"")
			require.ErrorContains(t, err, "policy failover does not allow filter")
			require.ErrorContains(t, err, "use primary and fallback")
		})
	}
}

func TestLegacyPriorityFailoverConfigsParseButFailBeforeGroupBuild(t *testing.T) {
	tests := []struct {
		name    string
		filters string
	}{
		{
			name: "two node",
			filters: `
    filter: name(A) [priority: 0]
    filter: name(X) [priority: 1]`,
		},
		{
			name: "rotating",
			filters: `
    filter: name(A) [priority: 0]
    filter: name(X) [priority: 1]
    filter: name(B) [priority: 2]
    filter: name(C) [priority: 3]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := `
global {}
routing { fallback: direct }
group {
  legacy {
    policy: failover` + tt.filters + `
  }
}`
			sections, err := config_parser.Parse(raw)
			require.NoError(t, err, "legacy syntax must reach semantic validation")
			conf, err := config.New(sections)
			require.NoError(t, err, "generic filter annotations still decode")
			require.Len(t, conf.Group, 1)

			policy, err := outbound.NewDialerSelectionPolicyFromGroupParam(&conf.Group[0])
			require.NoError(t, err)
			_, _, _, err = resolveConfiguredGroupDialers(nil, conf.Group[0], *policy)
			require.ErrorContains(t, err, "group \"legacy\"")
			require.ErrorContains(t, err, "policy failover does not allow filter")
			require.ErrorContains(t, err, "use primary and fallback")
		})
	}
}

func TestExplicitFailoverRolesWithOmittedPolicyFailDecode(t *testing.T) {
	raw := `
global {}
routing { fallback: direct }
group {
  omitted_policy {
    primary: name(A, B)
    fallback: name(X)
  }
}`
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	_, err = config.New(sections)
	require.Error(t, err)
	require.ErrorContains(t, err, "policy")
}

func TestFailoverRolesNeverInferPolicy(t *testing.T) {
	roles := config.Group{
		Name:     "g",
		Primary:  []*config_parser.Function{exactControlNameFunction("A")},
		Fallback: []*config_parser.Function{exactControlNameFunction("X")},
	}

	_, err := outbound.NewDialerSelectionPolicyFromGroupParam(&roles)
	require.Error(t, err, "role fields must not infer policy: failover")

	for _, policy := range []consts.DialerSelectionPolicy{
		consts.DialerSelectionPolicy_Random,
		consts.DialerSelectionPolicy_MinLastLatency,
	} {
		t.Run(string(policy), func(t *testing.T) {
			_, _, _, err := resolveConfiguredGroupDialers(
				nil,
				roles,
				outbound.DialerSelectionPolicy{Policy: policy},
			)
			require.ErrorContains(t, err, "group \"g\": primary and fallback require policy: failover")
		})
	}
}

func TestFailoverRoleErrorsIdentifyGroupRoleCauseAndValue(t *testing.T) {
	tests := []struct {
		name  string
		group config.Group
		want  []string
	}{
		{
			name: "wrong primary function",
			group: config.Group{
				Name:     "edge_group",
				Primary:  []*config_parser.Function{{Name: "subtag", Params: []*config_parser.Param{{Val: "A"}}}},
				Fallback: []*config_parser.Function{exactControlNameFunction("X")},
			},
			want: []string{"group \"edge_group\"", "primary", "exact name()", "subtag"},
		},
		{
			name: "keyed fallback",
			group: config.Group{
				Name:    "edge_group",
				Primary: []*config_parser.Function{exactControlNameFunction("A")},
				Fallback: []*config_parser.Function{{
					Name:   "name",
					Params: []*config_parser.Param{{Key: "regex", Val: "X.*"}},
				}},
			},
			want: []string{"group \"edge_group\"", "fallback", "must not use keys", "regex"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := resolveConfiguredGroupDialers(
				nil,
				withValidFailoverRecovery(tt.group),
				outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
			)
			require.Error(t, err)
			for _, fragment := range tt.want {
				require.ErrorContains(t, err, fragment)
			}
		})
	}
}
