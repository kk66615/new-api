package controller

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// GetUpstreamAuditHealth reports the upstream evidence writer's counters
// (inserted / dropped / failed, queue depth, last error, whether persistence
// is enabled) so the watchdog can alert when evidence stops being stored.
func GetUpstreamAuditHealth(c *gin.Context) {
	common.ApiSuccess(c, model.GetUpstreamEvidenceHealth())
}
