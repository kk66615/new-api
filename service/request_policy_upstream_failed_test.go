package service

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
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

func TestStreamFailedUpstreamSeesErrorWrittenIntoStartedStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.False(t, StreamFailedUpstream(c))

	// keepalive already committed 200; the final error goes into the stream status
	status := relaycommon.NewStreamStatus()
	status.MarkFailed("upstream_error", "upstream_error", 502)
	common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, status)
	assert.True(t, StreamFailedUpstream(c))
}

func TestBeginAttemptClearsUpstreamFailed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	failed := relaycommon.NewStreamStatus()
	failed.MarkFailed("server_error", "", 0)
	MarkRequestPolicySuccess(c, failed)
	assert.True(t, RequestPolicy(c).UpstreamFailed)

	RequestPolicy(c).BeginAttempt(&model.Channel{Id: 2}, "default")
	assert.False(t, RequestPolicy(c).UpstreamFailed, "a new attempt must not inherit the previous failure")
	MarkRequestPolicySuccess(c, nil)
	assert.False(t, RequestPolicy(c).UpstreamFailed)
}
