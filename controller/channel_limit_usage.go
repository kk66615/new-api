package controller

import (
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// MaxChannelLimitUsageIds bounds one usage request. The channel list asks in
// batches of this size (tag mode can list more channels than one page).
const MaxChannelLimitUsageIds = 200

// GetChannelLimitUsage returns the live use of the rate and quota limits of
// the channels named in ?ids=1,2,3 (the first MaxChannelLimitUsageIds
// entries). Channels without limits, unknown ids and malformed ids are left
// out. The limits are read from the same cached channels the limiter
// enforces, so the answer is what is being applied.
func GetChannelLimitUsage(c *gin.Context) {
	// Split off at most one entry beyond the cap, whatever the query's length.
	parts := strings.SplitN(c.Query("ids"), ",", MaxChannelLimitUsageIds+1)
	if len(parts) > MaxChannelLimitUsageIds {
		parts = parts[:MaxChannelLimitUsageIds]
	}
	seen := make(map[int]struct{}, len(parts))
	ids := make([]int, 0, len(parts))
	for _, raw := range parts {
		id, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || id <= 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	channels, err := model.CacheGetChannelsByIds(ids)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, model.ChannelLimitUsages(channels))
}
