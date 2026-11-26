package middleware

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"

	"github.com/gin-gonic/gin"
)

// Token RPM limiting (issue #7070): tokens.rpm_limit caps how many relay
// requests one API key may send in any 60 seconds. The rolling counter is
// shared by all nodes when Redis is enabled. It is consumed at admission —
// like the user-level ModelRequestRateLimit "total" counter, failed requests
// count too — while read-only endpoints that never route through this
// middleware (model listing, dashboard, token usage queries) stay uncounted.
// Task status queries are counted: some of them call the task's channel.
// A rejected request is not counted, so a client hammering the limit does not
// keep the window saturated.

const tokenRpmCounterPrefix = "tokenRPM"

// TokenRateLimit enforces the per-token RPM limit. Requests without a token
// (session-authenticated playground) or with limit 0 pass through untouched.
func TokenRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		enforceTokenRateLimit(c)
	}
}

// enforceTokenRateLimit counts one request against the token's rolling
// minute and aborts with 429 when the limit is exceeded.
func enforceTokenRateLimit(c *gin.Context) {
	tokenId := c.GetInt("token_id")
	if tokenId <= 0 {
		return
	}
	limit := int64(common.GetContextKeyInt(c, constant.ContextKeyTokenRpmLimit))
	if limit <= 0 {
		return
	}
	key := common.RollingCounterKey(tokenRpmCounterPrefix, tokenId)
	admitted, err := common.RollingCounterTryAdd(key, 1, limit)
	if err != nil {
		common.SysError("token rate limiter counter failure: " + err.Error())
		return
	}
	if !admitted {
		abortWithOpenAiMessage(c, http.StatusTooManyRequests,
			i18n.T(c, i18n.MsgTokenRpmLimitReached, map[string]any{"Limit": limit}))
	}
}
