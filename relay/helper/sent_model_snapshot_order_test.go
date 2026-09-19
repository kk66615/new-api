package helper

import (
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The audit compares the model we SENT against the model the upstream DECLARED.
// Getting the snapshot point wrong fails in two opposite ways, and this test
// pins both directions against the real ApplyReasoningModelSuffix:
//
//	too early  -> records "claude-3-7-sonnet-thinking" while "claude-3-7-sonnet"
//	              went out, so an upstream honestly echoing the request is
//	              reported as a model swapper (false positive, writes evidence)
//	too late   -> adopts the upstream's declared name, every comparison
//	              compares equal, and the audit silently stops working
func TestSentModelSnapshotMustBeTakenAfterSuffixHandling(t *testing.T) {
	newInfo := func() *relaycommon.RelayInfo {
		return &relaycommon.RelayInfo{
			OriginModelName: "claude-3-7-sonnet-thinking",
			ChannelMeta: &relaycommon.ChannelMeta{
				UpstreamModelName: "claude-3-7-sonnet-thinking",
			},
		}
	}

	t.Run("too early records the pre-strip name", func(t *testing.T) {
		info := newInfo()
		info.SnapshotUpstreamSentModel() // the position that was wrong
		mustApplyReasoningModelSuffix(t, info)

		require.Equal(t, "claude-3-7-sonnet", info.UpstreamModelName,
			"precondition: the suffix handler must strip the name")
		assert.Equal(t, "claude-3-7-sonnet-thinking", info.UpstreamSentModel,
			"documents the bug: an eagerly-taken snapshot keeps the suffix")
		assert.NotEqual(t, info.UpstreamModelName, info.UpstreamSentModel,
			"a drift here is what makes an honest upstream look like a swapper")
	})

	t.Run("at the send boundary records the name that actually went out", func(t *testing.T) {
		info := newInfo()
		mustApplyReasoningModelSuffix(t, info)
		info.SnapshotUpstreamSentModel() // the send-boundary position

		assert.Equal(t, "claude-3-7-sonnet", info.UpstreamSentModel)
		assert.Equal(t, info.UpstreamModelName, info.UpstreamSentModel,
			"the snapshot must agree with what the request carries")
	})

	t.Run("write-once means an early call cannot be repaired later", func(t *testing.T) {
		info := newInfo()
		info.SnapshotUpstreamSentModel() // too early, and it sticks
		mustApplyReasoningModelSuffix(t, info)
		info.SnapshotUpstreamSentModel() // send-boundary call is a no-op

		assert.Equal(t, "claude-3-7-sonnet-thinking", info.UpstreamSentModel,
			"write-once is why the call site must be right, not merely present")
	})
}

// An upstream that echoes back exactly the model it was sent must compare
// clean. This is the assertion the regression violated.
func TestHonestEchoOfStrippedModelComparesClean(t *testing.T) {
	info := &relaycommon.RelayInfo{
		OriginModelName: "claude-3-7-sonnet-thinking",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "claude-3-7-sonnet-thinking",
		},
	}
	mustApplyReasoningModelSuffix(t, info)
	info.SnapshotUpstreamSentModel()

	// the upstream honestly echoes the name it received
	info.ObserveUpstreamResponse(info.UpstreamSentModel, "msg_01", 0, "")

	assert.Equal(t, info.UpstreamSentModel, info.UpstreamResponseModel)
	assert.Equal(t, "claude-3-7-sonnet", info.UpstreamSentModel)
	assert.NotEqual(t, "claude-3-7-sonnet-thinking", info.UpstreamSentModel)
}
