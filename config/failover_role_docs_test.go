package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFailoverExampleUsesOnlyExplicitRoleTerminology(t *testing.T) {
	examplePath := filepath.Join("..", "example.dae")
	content, err := os.ReadFile(examplePath)
	require.NoError(t, err)

	example := string(content)
	require.NotContains(t, example, "priority failover")
	require.NotRegexp(t, `filter:.*\[priority:`, example)
	require.Contains(t, example, "primary: name(node_A)")
	require.Contains(t, example, "primary: name(node_A, node_B, node_C)")
	require.Contains(t, example, "fallback: name(xray_local)")
}
