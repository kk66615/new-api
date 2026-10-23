package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
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

// An upstream stream that ends before any data event fails the attempt with
// empty_response instead of settling on the local prompt estimate, and the
// handler writes nothing to the client (no synthesized usage chunk, terminal
// event or [DONE]), so the failure can still be answered cleanly.
func TestStreamHandlersFailEmptyUpstreamStreamWithoutWriting(t *testing.T) {
	require.NoError(t, i18n.Init())
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })

	for _, test := range []struct {
		name    string
		path    string
		mode    int
		format  types.RelayFormat
		handler func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{name: "chat completions", path: "/v1/chat/completions", mode: relayconstant.RelayModeChatCompletions, format: types.RelayFormatOpenAI, handler: OaiStreamHandler},
		{name: "responses", path: "/v1/responses", mode: relayconstant.RelayModeResponses, format: types.RelayFormatOpenAIResponses, handler: OaiResponsesStreamHandler},
		{name: "non-stream chat over a forced responses stream", path: "/v1/chat/completions", mode: relayconstant.RelayModeChatCompletions, format: types.RelayFormatOpenAI, handler: OaiResponsesToChatBufferedStreamHandler},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, test.path, nil)
			info := &relaycommon.RelayInfo{
				RelayMode:   test.mode,
				RelayFormat: test.format,
				IsStream:    true,
				ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "glm-5.3"},
			}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"up-empty-1"}},
				Body:       io.NopCloser(strings.NewReader(": keepalive\n\n")),
			}

			usage, apiErr := test.handler(c, info, resp)
			require.NotNil(t, apiErr)
			assert.Nil(t, usage)
			assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
			assert.False(t, c.Writer.Written(), "nothing may reach the client before the failure is answered")
			assert.Empty(t, recorder.Body.String())
			assert.Contains(t, info.UpstreamResponseHeaders, "up-empty-1")
			assert.Equal(t, ": keepalive", info.UpstreamResponseExcerpt)
		})
	}
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

// The Azure content-safety fields sit in the first chunk (empty choices and
// model) and the content chunks, not in the final usage chunk: the stream
// handler must scan every frame.
func TestOaiStreamHandlerScansEveryFrameForAzureMarkers(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeChatCompletions,
		RelayFormat: types.RelayFormatOpenAI,
		IsStream:    true,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o"},
	}
	frames := []string{
		`{"choices":[],"created":0,"id":"","model":"","object":"","prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}]}`,
		`{"choices":[{"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}},"delta":{"content":"Hi"},"index":0}],"created":1790000000,"id":"chatcmpl-AzUre0002","model":"gpt-4o-2024-11-20","object":"chat.completion.chunk"}`,
		`{"choices":[],"created":1790000000,"id":"chatcmpl-AzUre0002","model":"gpt-4o-2024-11-20","object":"chat.completion.chunk","usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`,
	}
	var sse strings.Builder
	for _, f := range frames {
		sse.WriteString("data: " + f + "\n\n")
	}
	sse.WriteString("data: [DONE]\n\n")
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Oneapi-Request-Id": {"x"}}, Body: io.NopCloser(strings.NewReader(sse.String()))}

	_, apiErr := OaiStreamHandler(newAuditTestContext("/v1/chat/completions"), info, resp)
	require.Nil(t, apiErr)
	assert.True(t, info.UpstreamAudit.BodyMarkers.Has(relaycommon.BodyMarkerAzureContentFilter))
	assert.NotContains(t, info.UpstreamResponseExcerpt, "content_filter", "the excerpt is the marker-free usage chunk")
}
