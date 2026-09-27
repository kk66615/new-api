package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAuditTestContext(path string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	return c
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}, "X-Upstream": {"a", "b"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// The evidence keeps the raw upstream bytes: the OpenRouter enterprise
// envelope before it is unwrapped (spec E3).
func TestOpenaiHandlerKeepsRawOpenRouterEnvelope(t *testing.T) {
	enterprise := true
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatOpenAI,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5", ChannelType: constant.ChannelTypeOpenRouter},
	}
	info.ChannelOtherSettings.OpenRouterEnterprise = &enterprise
	raw := `{"success":true,"data":{"id":"gen-abc123456","object":"chat.completion","model":"openai/gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}}`

	_, apiErr := OpenaiHandler(newAuditTestContext("/v1/chat/completions"), info, jsonResponse(raw))
	require.Nil(t, apiErr)
	assert.Equal(t, raw, info.UpstreamResponseExcerpt)
	assert.Equal(t, "openai/gpt-5", info.UpstreamResponseModel)
	assert.Equal(t, "openai_chat", info.UpstreamAudit.Handler)
	assert.Contains(t, info.UpstreamResponseHeaders, `"X-Upstream":"a, b"`)
	assert.Len(t, info.UpstreamAudit.OutputSHA256, 64)
}

// The SGLang created_at rewrite changes the client body, not the evidence.
func TestOaiResponsesHandlerKeepsRawCreatedAtForSGLang(t *testing.T) {
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "qwen3", ChannelType: constant.ChannelTypeSGLang}}
	raw := `{"id":"resp_abc123456","object":"response","created_at":"1788619663","model":"qwen3","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	_, apiErr := OaiResponsesHandler(c, info, jsonResponse(raw))
	require.Nil(t, apiErr)
	assert.Contains(t, recorder.Body.String(), `"created_at":1788619663`, "the client receives the rewritten value")
	assert.Equal(t, raw, info.UpstreamResponseExcerpt, "the evidence keeps the upstream's own bytes")
}

// A stream that ends with a usage-only frame without a model keeps the frame
// that declared the model; every distinct model is recorded (spec D1, D2).
func TestOaiStreamHandlerKeepsModelFrameBehindUsageOnlyFrame(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeChatCompletions,
		RelayFormat: types.RelayFormatOpenAI,
		IsStream:    true,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5"},
	}
	first := `{"id":"chatcmpl-s1234567","object":"chat.completion.chunk","created":1788619663,"model":"gpt-5","choices":[{"index":0,"delta":{"content":"hello"}}]}`
	second := `{"id":"chatcmpl-s1234567","object":"chat.completion.chunk","created":1788619663,"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	usageOnly := `{"id":"chatcmpl-s1234567","object":"chat.completion.chunk","created":1788619663,"model":"","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	sse := "data: " + first + "\n\ndata: " + second + "\n\ndata: " + usageOnly + "\n\ndata: [DONE]\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}

	usage, apiErr := OaiStreamHandler(newAuditTestContext("/v1/chat/completions"), info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, first, info.UpstreamAudit.ModelFrame)
	assert.Equal(t, usageOnly, info.UpstreamResponseExcerpt)
	assert.Equal(t, "gpt-5", info.UpstreamResponseModel)
	assert.Equal(t, []string{"gpt-5"}, info.UpstreamAudit.Models)
	assert.Equal(t, "openai_chat_stream", info.UpstreamAudit.Handler)
	assert.Equal(t, "full", info.UpstreamObservationLevel())
}
