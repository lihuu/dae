package dialer

import (
	"testing"
	"time"

	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

func TestNewAnnotationKeepsAddLatencyAndRejectsLegacyPriority(t *testing.T) {
	t.Run("add_latency remains supported", func(t *testing.T) {
		annotation, err := NewAnnotation([]*config_parser.Param{{
			Key: AnnotationKey_AddLatency,
			Val: "-500ms",
		}})
		require.NoError(t, err)
		require.Equal(t, -500*time.Millisecond, annotation.AddLatency)
	})

	t.Run("legacy priority is rejected", func(t *testing.T) {
		_, err := NewAnnotation([]*config_parser.Param{{
			Key: "priority",
			Val: "0",
		}})
		require.ErrorContains(t, err, "unknown filter annotation: priority")
	})
}
