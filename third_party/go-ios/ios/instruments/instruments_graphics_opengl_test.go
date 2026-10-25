package instruments

import (
	"testing"

	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapToFramesPerSecondSample(t *testing.T) {
	msg := dtx.Message{
		Payload: []interface{}{
			map[string]interface{}{
				"CoreAnimationFramesPerSecond": float64(59.8),
			},
		},
	}

	sample, err := mapToFramesPerSecondSample(msg)

	require.NoError(t, err)
	assert.InDelta(t, 59.8, sample.CoreAnimationFramesPerSecond, 0.001)
}

func TestMapToFramesPerSecondSampleAcceptsNumericTypes(t *testing.T) {
	msg := dtx.Message{
		Payload: []interface{}{
			map[string]interface{}{
				"CoreAnimationFramesPerSecond": int(60),
			},
		},
	}

	sample, err := mapToFramesPerSecondSample(msg)

	require.NoError(t, err)
	assert.Equal(t, float64(60), sample.CoreAnimationFramesPerSecond)
}

func TestMapToFramesPerSecondSampleErrors(t *testing.T) {
	tests := []struct {
		name      string
		msg       dtx.Message
		errSubstr string
	}{
		{"empty payload", dtx.Message{}, "empty"},
		{"bad payload", dtx.Message{Payload: []interface{}{"payload"}}, "map[string]interface{}"},
		{"missing fps", dtx.Message{Payload: []interface{}{map[string]interface{}{}}}, "CoreAnimationFramesPerSecond"},
		{"bad fps", dtx.Message{Payload: []interface{}{map[string]interface{}{"CoreAnimationFramesPerSecond": "60"}}}, "numeric"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mapToFramesPerSecondSample(tt.msg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}
