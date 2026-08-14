package helper

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newBillingExprRequestContext(t *testing.T, body []byte) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Body = io.NopCloser(bytes.NewReader(body))
	ctx.Set(common.KeyRequestBody, body)
	return ctx
}

func TestResolveIncomingBillingExprRequestInput(t *testing.T) {
	body := []byte(`{"service_tier":"fast"}`)
	ctx := newBillingExprRequestContext(t, body)

	info := &relaycommon.RelayInfo{
		RequestHeaders: map[string]string{"Content-Type": "application/json"},
	}

	input, err := ResolveIncomingBillingExprRequestInput(ctx, info, `p * (param("service_tier") == "priority" ? 2 : 1)`)
	require.NoError(t, err)
	require.Equal(t, body, input.Body)
	require.Equal(t, "application/json", input.Headers["Content-Type"])
}

// TestResolveIncomingBillingExprRequestInputSkipsBodyWithoutParam pins down why
// the exprStr argument exists. The returned Body is retained on
// info.BillingRequestInput until settlement, and for a disk-backed body
// storage.Bytes() ReadFulls the whole payload into a fresh heap buffer, so
// expressions that never call param() must not pay for that copy.
func TestResolveIncomingBillingExprRequestInputSkipsBodyWithoutParam(t *testing.T) {
	body := []byte(`{"service_tier":"fast"}`)

	for name, exprStr := range map[string]string{
		"tiered":       `len <= 200000 ? tier("standard", p * 1.25 + c * 10) : tier("long_context", p * 2.5 + c * 15)`,
		"header only":  `p * (header("x-fast") == "1" ? 2 : 1)`,
		"empty":        ``,
		"uncompilable": `p * (`,
	} {
		t.Run(name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				RequestHeaders: map[string]string{"Content-Type": "application/json"},
			}
			input, err := ResolveIncomingBillingExprRequestInput(newBillingExprRequestContext(t, body), info, exprStr)
			require.NoError(t, err)
			require.Empty(t, input.Body)
			// Headers stay unconditional: they are cheap and header() needs them.
			require.Equal(t, "application/json", input.Headers["Content-Type"])
		})
	}
}

func TestBuildBillingExprRequestInputFromRequest(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{
		Model:  "gemini-3.1-pro-preview",
		Stream: lo.ToPtr(true),
		Messages: []dto.Message{
			{
				Role:    "user",
				Content: "hi",
			},
		},
		MaxTokens: lo.ToPtr(uint(3000)),
	}

	input, err := BuildBillingExprRequestInputFromRequest(request, map[string]string{
		"Content-Type": "application/json",
		"X-Test":       "1",
	})
	require.NoError(t, err)
	require.Equal(t, "application/json", input.Headers["Content-Type"])
	require.Equal(t, "1", input.Headers["X-Test"])
	require.True(t, gjson.GetBytes(input.Body, "stream").Bool())
	require.Equal(t, "user", gjson.GetBytes(input.Body, "messages.0.role").String())
	require.Equal(t, float64(3000), gjson.GetBytes(input.Body, "max_tokens").Float())
}
