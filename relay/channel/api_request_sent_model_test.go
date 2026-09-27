package channel

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestDoRequestSnapshotsTheModelTheUpstreamReceived sends real requests
// through doRequest to an httptest upstream and asserts that the audit's
// sent-model snapshot equals the model the upstream actually received — in
// the body, or in the URL path for Gemini-style endpoints — including the
// pass-through and param-override cases where UpstreamModelName is not what
// went out.
func TestDoRequestSnapshotsTheModelTheUpstreamReceived(t *testing.T) {
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)

	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		model := gjson.GetBytes(body, "model").String()
		if model == "" {
			model = "path:" + r.URL.Path
		}
		received <- model
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	send := func(t *testing.T, info *relaycommon.RelayInfo, path string, body []byte, replayable bool) {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		var reader io.Reader = bytes.NewReader(body)
		if replayable {
			outbound, closer, err := relaycommon.NewOutboundJSONBody(body)
			require.NoError(t, err)
			t.Cleanup(func() { _ = closer.Close() })
			reader = outbound
		}
		req, err := http.NewRequest(http.MethodPost, upstream.URL+path, reader)
		require.NoError(t, err)
		ApplyUpstreamBodyMetadata(req, reader)
		req.Header.Set("Content-Type", "application/json")
		resp, err := doRequest(ctx, req, info)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	t.Run("converted body carries the mapped model", func(t *testing.T) {
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-3-7-sonnet"}}
		send(t, info, "/v1/messages", []byte(`{"model":"claude-3-7-sonnet","messages":[]}`), false)
		got := <-received
		assert.Equal(t, got, info.UpstreamSentModel)
		assert.Empty(t, info.UpstreamAudit.SentField)
	})

	t.Run("pass-through body keeps the client model despite mapping", func(t *testing.T) {
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-5-mapped", IsModelMapped: true,
		}}
		send(t, info, "/v1/chat/completions", []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"gpt-5"}`), true)
		got := <-received
		assert.Equal(t, "gpt-5", got)
		assert.Equal(t, got, info.UpstreamSentModel)
		assert.Equal(t, "gpt-5-mapped", info.UpstreamAudit.SentField)
	})

	t.Run("param override rewriting model", func(t *testing.T) {
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-5",
			ParamOverride:     map[string]any{"model": "gpt-5-overridden"},
		}}
		body, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-5","messages":[]}`), info)
		require.NoError(t, err)
		send(t, info, "/v1/chat/completions", body, true)
		got := <-received
		assert.Equal(t, "gpt-5-overridden", got)
		assert.Equal(t, got, info.UpstreamSentModel)
		assert.Equal(t, "gpt-5", info.UpstreamAudit.SentField)
	})

	t.Run("gemini path model when the body has none", func(t *testing.T) {
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gemini-2.5-pro"}}
		send(t, info, "/v1beta/models/gemini-2.5-flash:generateContent", []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`), false)
		got := <-received
		assert.Equal(t, "path:/v1beta/models/gemini-2.5-flash:generateContent", got)
		assert.Equal(t, "gemini-2.5-flash", info.UpstreamSentModel)
		assert.Equal(t, "gemini-2.5-pro", info.UpstreamAudit.SentField)
	})

	t.Run("prompt hash and temperature come from the sent body", func(t *testing.T) {
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5"}}
		body := []byte(`{"model":"gpt-5","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)
		send(t, info, "/v1/chat/completions", body, false)
		<-received
		require.NotNil(t, info.UpstreamAudit.Temperature)
		assert.InDelta(t, 0.7, *info.UpstreamAudit.Temperature, 1e-9)
		reordered := []byte(`{"messages":[{"content":"hi","role":"user"}],"model":"other"}`)
		assert.Equal(t, relaycommon.CanonicalPromptSHA256(reordered), info.UpstreamAudit.PromptSHA256,
			"key order must not change the prompt hash")
		assert.Len(t, info.UpstreamAudit.PromptSHA256, 64)
	})
}

// A failing first attempt on another channel must not leave its snapshot on
// the retry: the retry's snapshot is what its own upstream received.
func TestDoRequestSnapshotIsPerAttempt(t *testing.T) {
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)
	received := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- gjson.GetBytes(body, "model").String()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "a"}}
	for _, model := range []string{"model-a", "model-b"} {
		info.ResetUpstreamObservation() // InitChannelMeta per attempt
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		body, err := common.Marshal(map[string]any{"model": model})
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, upstream.URL, bytes.NewReader(body))
		require.NoError(t, err)
		resp, err := doRequest(ctx, req, info)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, <-received, info.UpstreamSentModel)
	}
}
