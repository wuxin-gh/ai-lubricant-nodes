package instruments

import (
	"testing"

	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapToNetworkSample(t *testing.T) {
	msg := dtx.Message{
		Payload: []interface{}{
			uint64(2),
			map[string]interface{}{
				"interfaceName": "en0",
				"rxBytes":       uint64(42),
			},
		},
	}

	sample, err := mapToNetworkSample(msg)

	require.NoError(t, err)
	assert.Equal(t, uint64(2), sample.Type)
	assert.Equal(t, "en0", sample.Data["interfaceName"])
	assert.Equal(t, uint64(42), sample.Data["rxBytes"])
}

func TestMapToNetworkSampleAcceptsNumericTypes(t *testing.T) {
	msg := dtx.Message{
		Payload: []interface{}{
			int(2),
			map[string]interface{}{},
		},
	}

	sample, err := mapToNetworkSample(msg)

	require.NoError(t, err)
	assert.Equal(t, uint64(2), sample.Type)
}

func TestMapToNetworkSampleErrors(t *testing.T) {
	tests := []struct {
		name      string
		msg       dtx.Message
		errSubstr string
	}{
		{"empty payload", dtx.Message{}, "at least two elements"},
		{"missing data", dtx.Message{Payload: []interface{}{uint64(2)}}, "at least two elements"},
		{"bad type", dtx.Message{Payload: []interface{}{"2", map[string]interface{}{}}}, "sample type"},
		{"bad data", dtx.Message{Payload: []interface{}{uint64(2), "payload"}}, "sample data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mapToNetworkSample(tt.msg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}
