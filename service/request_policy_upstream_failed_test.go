package service

import (
	"errors"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestMarkRequestPolicySuccessFlagsOnlyUpstreamFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name       string
		stream     func() *relaycommon.StreamStatus
		wantFailed bool
	}{
		{
			name:       "non-stream success",
			stream:     func() *relaycommon.StreamStatus { return nil },
			wantFailed: false,
		},
		{
			name: "upstream response.failed",
			stream: func() *relaycommon.StreamStatus {
				s := relaycommon.NewStreamStatus()
				s.MarkFailed("server_error", "", 0)
				s.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
				return s
			},
			wantFailed: true,
		},
		{
			name: "upstream connection broke",
			stream: func() *relaycommon.StreamStatus {
				s := relaycommon.NewStreamStatus()
				s.SetEndReason(relaycommon.StreamEndReasonScannerErr, errors.New("stream error"))
				return s
			},
			wantFailed: true,
		},
		{
			name: "client hung up after completion",
			stream: func() *relaycommon.StreamStatus {
				s := relaycommon.NewStreamStatus()
				s.MarkCompleted()
				s.SetEndReason(relaycommon.StreamEndReasonClientGone, errors.New("context canceled"))
				return s
			},
			wantFailed: false,
		},
		{
			name: "client hung up mid stream",
			stream: func() *relaycommon.StreamStatus {
				s := relaycommon.NewStreamStatus()
				s.SetEndReason(relaycommon.StreamEndReasonClientGone, errors.New("context canceled"))
				return s
			},
			wantFailed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			MarkRequestPolicySuccess(c, tc.stream())
			assert.Equal(t, tc.wantFailed, RequestPolicy(c).UpstreamFailed)
		})
	}
}
