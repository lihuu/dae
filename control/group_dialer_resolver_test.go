package control

import (
	"testing"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/outbound"
	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

func exactControlNameFunction(value string) *config_parser.Function {
	return &config_parser.Function{
		Name:   "name",
		Params: []*config_parser.Param{{Val: value}},
	}
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
