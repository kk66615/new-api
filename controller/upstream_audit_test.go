package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The watchdog polls this contract: {"success":true,"data":{counters...}}.
func TestGetUpstreamAuditHealthContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/audit/health", nil)

	GetUpstreamAuditHealth(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var body struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
	assert.True(t, body.Success)
	for _, key := range []string{"inserted_total", "dropped_total", "failed_total", "last_error", "last_error_at",
		"queue_len", "queue_cap", "started_at", "enabled"} {
		assert.Contains(t, body.Data, key)
	}
}
