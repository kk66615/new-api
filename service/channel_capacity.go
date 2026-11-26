package service

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

// maxChannelCapacitySkips bounds how many dispatches one request may hand on
// after local capacity refusals. A refused channel is at its limit, so the
// selection filter leaves it out next time and a request normally ends after
// at most one refusal per candidate; the bound only stops a loop in which
// rolling windows keep reopening and refilling between attempts.
const maxChannelCapacitySkips = 16

const ginKeyChannelCapacitySkips = "channel_capacity_skips"

// ChannelAtCapacityError is the error of a dispatch refused by a local channel
// limit (RPM, TPM or quota budget). Its message names neither the channel nor
// the limit: it can reach the client, and channel ids stay private.
func ChannelAtCapacityError(c *gin.Context) *types.NewAPIError {
	return types.NewErrorWithStatusCode(
		errors.New(i18n.T(c, i18n.MsgRelayChannelAtCapacity)),
		model.ChannelLimitExceededCode,
		http.StatusTooManyRequests,
	)
}

// SkipChannelAtCapacity handles a dispatch that the channel's own limit
// refused before anything was sent upstream, and reports whether the request
// may move on to another channel. The refusal is not an upstream failure, so
// it neither uses up the retry budget nor depends on the retryable status
// codes: only the rules that keep a request on its channel (client gone,
// strict session, pinned channel) stop it. When it moves on, the refused
// channel joins the use_channel trail and the retry counter is held, so the
// next selection stays in the same priority tier.
func SkipChannelAtCapacity(c *gin.Context, channelID int, err *types.NewAPIError, retry *RetryParam) bool {
	decision := PolicyDecision{Action: "retry", Reason: "channel_at_capacity", Source: "system"}
	if stay, stop := relayMustStayOnChannel(c); stop {
		decision = stay
	} else if c.GetInt(ginKeyChannelCapacitySkips) >= maxChannelCapacitySkips {
		decision = PolicyDecision{Action: "stop", Reason: "capacity_skips_exhausted", Source: "system"}
	}
	RecordPolicyFailure(c, channelID, err, decision)
	if decision.Action != "retry" {
		return false
	}
	c.Set(ginKeyChannelCapacitySkips, c.GetInt(ginKeyChannelCapacitySkips)+1)
	AppendUsedChannel(c, channelID)
	retry.ResetRetryNextTry()
	return true
}

// ChannelsOverLimitSelectError is the selection error when every candidate
// is over its limits (or a pinned channel is): 429 channel_limit_exceeded.
func ChannelsOverLimitSelectError(group string, modelName string) *ChannelSelectError {
	return &ChannelSelectError{
		StatusCode: http.StatusTooManyRequests, Code: model.ChannelLimitExceededCode,
		MessageID: i18n.MsgDistributorChannelsOverLimit,
		Params:    map[string]any{"Group": group, "Model": modelName},
	}
}
