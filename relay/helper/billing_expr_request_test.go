package helper

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestResolveIncomingBillingExprRequestInput pins the two contracts the caller
// relies on: an expression that reads param() gets the frozen body, and one
// that does not leaves it unread so a disk-backed body is never copied into
// memory for the whole upstream response.
func TestResolveIncomingBillingExprRequestInput(t *testing.T) {
	body := []byte(`{"service_tier":"fast"}`)
	for _, tc := range []struct {
		name      string
		frozen    *billingexpr.RequestInput
		readsBody bool
		wantBody  []byte
	}{
		{name: "param expression reads the body", readsBody: true, wantBody: body},
		{name: "expression without param leaves the body unread"},
		{name: "later param expression reads a body the snapshot skipped", frozen: &billingexpr.RequestInput{}, readsBody: true, wantBody: body},
		{name: "frozen body is kept", frozen: &billingexpr.RequestInput{Body: []byte(`{"service_tier":"flex"}`)}, readsBody: true, wantBody: []byte(`{"service_tier":"flex"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			t.Cleanup(func() { common.CleanupBodyStorage(ctx) })
			info := &relaycommon.RelayInfo{
				RequestHeaders:      map[string]string{"Content-Type": "application/json"},
				BillingRequestInput: tc.frozen,
			}

			input, err := ResolveIncomingBillingExprRequestInput(ctx, info, tc.readsBody)
			require.NoError(t, err)
			assert.Equal(t, tc.wantBody, input.Body)
			assert.Equal(t, "application/json", input.Headers["Content-Type"])
		})
	}
}

// TestBillingExprParamVarDecidesBodyRead pins why the caller passes
// usedVars["param"]: only expressions that call param() may cause the body to
// be read, and an unparsable expression must not read it either.
func TestBillingExprParamVarDecidesBodyRead(t *testing.T) {
	for name, exprStr := range map[string]bool{
		"tiered":                           false,
		"header only":                      false,
		"empty":                            false,
		"uncompilable":                     false,
		"param service tier":               true,
		"param inside a tiered expression": true,
	} {
		t.Run(name, func(t *testing.T) {
			expr := map[string]string{
				"tiered":                           `len <= 200000 ? tier("standard", p * 1.25 + c * 10) : tier("long_context", p * 2.5 + c * 15)`,
				"header only":                      `p * (header("x-fast") == "1" ? 2 : 1)`,
				"empty":                            ``,
				"uncompilable":                     `p * (`,
				"param service tier":               `p * (param("service_tier") == "priority" ? 2 : 1)`,
				"param inside a tiered expression": `tier("std", p * 1 + (param("n") > 1 ? c * 2 : 0))`,
			}[name]
			assert.Equal(t, exprStr, billingexpr.UsedVars(expr)["param"])
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
	require.True(t, ojson.GetBytes(input.Body, "stream").Bool())
	require.Equal(t, "user", ojson.GetBytes(input.Body, "messages.0.role").String())
	require.Equal(t, float64(3000), ojson.GetBytes(input.Body, "max_tokens").Float())
}
