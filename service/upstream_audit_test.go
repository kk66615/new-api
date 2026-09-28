package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/audit_setting"
	"github.com/QuantumNous/new-api/setting/config"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpstreamModelsMatchForAudit(t *testing.T) {
	cases := []struct {
		sent, resp string
		want       bool
	}{
		// exact / case-insensitive
		{"deepseek-v4-pro", "deepseek-v4-pro", true},
		{"GPT-5", "gpt-5", true},
		// tri-state: missing data never flags
		{"", "anything", true},
		{"gpt-5", "", true},
		// OpenAI alias ↔ snapshot
		{"gpt-5", "gpt-5-2025-08-07", true},
		{"gpt-5.2", "gpt-5.2-2025-12-11", true},
		{"gpt-5-2025-08-07", "gpt-5", true}, // reversed: pinned_to_alias is recorded, not a mismatch
		// DeepSeek date build
		{"deepseek-v4-flash", "deepseek-v4-flash-0731", true},
		// -latest alias (suffix form)
		{"claude-sonnet-4-6", "claude-sonnet-4-6-latest", true},
		// -latest rolling alias resolving to a concrete same-family version (2026-09-03 #57 real traffic)
		{"gemini-pro-latest", "gemini-3.1-pro-preview", true}, // Google ships -latest targets as -preview builds
		{"gemini-flash-latest", "gemini-3.7-flash", true},
		{"gemini-flash-latest", "gemini-3.1-pro-preview", false}, // tier swap must still flag
		{"gemini-pro-latest", "gemini-2.5-flash-lite", false},    // tier swap must still flag
		// grok runtime build ids
		{"grok-4.5", "grok-4.5-build", true},
		{"grok-4.6-latest", "grok-4.6-build", true},
		// OpenRouter-style vendor/ prefix echoes
		{"deepseek-v4-flash", "deepseek/deepseek-v4-flash", true},
		{"claude-sonnet-4-6", "anthropic/claude-sonnet-4-6", true},
		{"gpt-5", "openai/gpt-5-2025-08-07", true},
		// REAL mismatches must still flag (2026-09-03 #114 real case: 5.5 swapped for 5.6-luna)
		{"gpt-5.5", "gpt-5-6-luna", false},
		{"gpt-5.5", "openai/gpt-5-6-luna", false},
		{"claude-opus-4-7", "deepseek-v4-pro", false},
		{"gpt-5", "gpt-5-mini", false},                  // non-date suffix
		{"deepseek-v4", "deepseek-v4-flash", false},     // different model, prefix relation
		{"gpt-5", "gpt-5-turbo-2024", false},            // mixed suffix
		{"deepseek-v4-pro", "deepseek-v4-flash", false}, // sibling swap
		{"gpt-5", "gpt-5-123456789", false},             // digits too long
	}
	for _, c := range cases {
		if got := UpstreamModelsMatchForAudit(c.sent, c.resp); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.sent, c.resp, got, c.want)
		}
	}
}

// TestClassifyUpstreamModelMismatch pins the verdict labels. The boolean above
// is derived from this classifier, so every case here must agree with the
// corresponding row in TestUpstreamModelsMatchForAudit.
func TestClassifyUpstreamModelMismatch(t *testing.T) {
	cases := []struct {
		sent, resp string
		want       string
	}{
		{"deepseek-v4-pro", "deepseek-v4-pro", VerdictExact},
		{"GPT-5", "gpt-5", VerdictExact},
		// tri-state: missing data is its own verdict and never a mismatch
		{"", "anything", VerdictNoModel},
		{"gpt-5", "", VerdictNoModel},
		{"", "", VerdictNoModel},
		// vendor namespace only
		{"deepseek-v4-flash", "deepseek/deepseek-v4-flash", VerdictVendorPrefix},
		// "-preview" public name reported as the bare id (Google image models, #168)
		{"gemini-3-pro-image-preview", "gemini-3-pro-image", VerdictPreviewAlias},
		{"gemini-3.1-flash-image-preview", "google/gemini-3.1-flash-image", VerdictPreviewAlias},
		{"Gemini-3-Pro-Image-Preview", "gemini-3-pro-image", VerdictPreviewAlias},
		// the reverse direction is a substituted build, still a mismatch
		{"gemini-3-pro-image", "gemini-3-pro-image-preview", VerdictMismatch},
		// only a whole trailing token counts; a different base stays a mismatch
		{"gemini-3-pro-image-preview", "gemini-3-flash-image", VerdictMismatch},
		{"-preview", "x", VerdictMismatch}, // reaches the length guard
		// outside the gemini family the bare name is a different product
		{"o1-preview", "o1", VerdictMismatch},
		{"gpt-4.5-preview", "gpt-4.5", VerdictMismatch},
		{"claude-sonnet-4-6", "anthropic/claude-sonnet-4-6", VerdictVendorPrefix},
		{"claude-sonnet-5", "global.anthropic.claude-sonnet-5", VerdictVendorPrefix},
		// vendor runtime alias
		{"grok-4.5", "grok-4.5-build", VerdictRuntimeAlias},
		{"grok-4.6-latest", "grok-4.6-build", VerdictRuntimeAlias},
		// alias ↔ dated snapshot
		{"gpt-5", "gpt-5-2025-08-07", VerdictSnapshotDate},
		{"deepseek-v4-flash", "deepseek-v4-flash-0731", VerdictSnapshotDate},
		// rolling -latest alias
		{"gemini-pro-latest", "gemini-3.1-pro-preview", VerdictRollingAlias}, // preview is not a downgrade for -latest
		{"claude-sonnet-4-6", "claude-sonnet-4-6-latest", VerdictRollingAlias},
		// real mismatches
		{"gpt-5.5", "gpt-5-6-luna", VerdictMismatch},
		{"claude-opus-4-7", "deepseek-v4-pro", VerdictMismatch},
		{"gpt-5", "gpt-5-mini", VerdictMismatch},
		{"deepseek-v4", "deepseek-v4-flash", VerdictMismatch},
		{"gemini-flash-latest", "gemini-3.1-pro-preview", VerdictMismatch},
	}
	for _, c := range cases {
		if got := ClassifyUpstreamModelMismatch(c.sent, c.resp); got != c.want {
			t.Errorf("Classify(%q, %q) = %q, want %q", c.sent, c.resp, got, c.want)
		}
	}
}

// TestUpstreamModelIsTierDowngrade covers the highest-signal adulteration shape:
// upstream serving a weaker sibling of the same family.
//
// The label never feeds the verdict, so a false negative here is a cosmetic
// loss; a false POSITIVE shows the operator a wrong conclusion, which is worse.
// The dated-preview rows below exist because an earlier substring
// implementation mislabelled them as downgrades.
func TestUpstreamModelIsTierDowngrade(t *testing.T) {
	cases := []struct {
		sent, resp string
		want       bool
		why        string
	}{
		// additive downgrade: response carries a weaker qualifier
		{"deepseek-v4", "deepseek-v4-flash", true, "flash tier"},
		// subtractive -preview is an accepted alias (preview_alias), never a downgrade
		{"gemini-3-pro-image-preview", "gemini-3-pro-image", false, "preview alias, not a tier change"},
		{"gpt-5", "gpt-5-mini", true, "mini tier"},
		{"gemini-2.5-pro", "gemini-2.5-pro-preview", true, "preview tier"},
		{"claude-sonnet-5", "claude-sonnet-5-lite", true, "lite tier"},
		{"gpt-5", "openai/gpt-5-nano", true, "nano tier, vendor prefix stripped"},
		// subtractive downgrade: response drops one of our higher-tier qualifiers
		{"deepseek-v4-pro", "deepseek-v4", true, "pro qualifier dropped"},
		{"gpt-5-pro", "gpt-5", true, "pro qualifier dropped"},
		{"claude-opus-4-7-thinking", "claude-opus-4-7", true, "thinking dropped"},
		// A tier token decides the grade regardless of any date build number
		// riding along with it: "-preview" is a preview build, and its date does
		// not upgrade it. Only a bare date (no tier word) is a same-grade pin.
		{"gpt-5", "gpt-5-preview-2025-08-07", true, "preview grade, dated"},
		{"deepseek-v4", "deepseek-v4-flash-0731", true, "flash grade, dated build"},
		{"gpt-5", "gpt-5-mini-2025-08-07", true, "mini grade, dated"},
		// bare date with no tier word is a version pin, not a grade change
		{"gpt-5", "gpt-5-2025-08-07", false, "dated snapshot, same grade"},
		{"claude-sonnet-5", "claude-sonnet-5-20260101", false, "dated snapshot, same grade"},
		{"gpt-5", "gpt-5-pro-2025-08-07", false, "no lower-tier word; a naming diff"},
		// unrelated or sibling models
		{"deepseek-v4-pro", "deepseek-v4-flash", true, "replacement: pro replaced by flash"},
		{"claude-opus-4-7", "deepseek-v4-pro", false, "unrelated model"},
		{"grok-4.5", "grok-4.5-build", false, "runtime alias, not a tier"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6-latest", false, "rolling alias"},
		{"gemini-pro-latest", "gemini-3.1-pro-preview", false, "alias resolution, not a mismatch"},
		// version-only change within the same tier: a different model, not a tier swap
		{"claude-opus-4-7", "claude-opus-4-5", false, "version change, tier unknown"},
		// degenerate input
		{"", "gpt-5-mini", false, "no sent model"},
		{"gpt-5", "", false, "no response model"},
		{"gpt-5", "gpt-5", false, "identical"},
	}
	for _, c := range cases {
		if got := UpstreamModelIsTierDowngrade(c.sent, c.resp); got != c.want {
			t.Errorf("TierDowngrade(%q, %q) = %v, want %v (%s)", c.sent, c.resp, got, c.want, c.why)
		}
	}
}

// TestTierDowngradeNeverAffectsVerdict pins the invariant the log relies on:
// the tier label is display-only and can never turn a clean pair into an
// anomaly, nor an anomaly into a clean one. It is enforced structurally by the
// "mismatch &&" gate at the call site; this test fails loudly if that gate is
// ever dropped.
func TestTierDowngradeNeverAffectsVerdict(t *testing.T) {
	models := []string{
		"", "gpt-5", "gpt-5-mini", "gpt-5-preview-2025-08-07", "gpt-5-pro",
		"deepseek-v4", "deepseek-v4-flash", "deepseek-v4-pro",
		"claude-opus-4-7", "claude-opus-4-5", "claude-sonnet-4-6-latest",
		"openai/gpt-5-nano", "grok-4.5", "grok-4.5-build", "gemini-pro-latest",
	}
	for _, sent := range models {
		for _, resp := range models {
			tier := UpstreamModelIsTierDowngrade(sent, resp)
			if !tier {
				continue // only labelled pairs are under test
			}
			if verdict := ClassifyUpstreamModelMismatch(sent, resp); !VerdictIsMismatch(verdict) {
				t.Fatalf("tier downgrade %q -> %q has verdict %q; the label must never stand alone",
					sent, resp, verdict)
			}
		}
	}
}

// TestVerdictHardening pins the stricter verdict rules (spec A1–A5), including
// every reviewer counter-example: a pinned build is only accepted in the
// alias→build direction, "dates" are range-checked, -latest cannot hide a
// lower tier, and Bedrock build suffixes are compared when both sides carry one.
func TestVerdictHardening(t *testing.T) {
	cases := []struct {
		name, sent, resp, want string
	}{
		// A1 strict date forms, alias → dated build
		{"openai YYYY-MM-DD", "gpt-5", "gpt-5-2025-08-07", VerdictSnapshotDate},
		{"anthropic YYYYMMDD", "claude-haiku-4-5", "claude-haiku-4-5-20251001", VerdictSnapshotDate},
		{"deepseek MMDD", "deepseek-v4-flash", "deepseek-v4-flash-0731", VerdictSnapshotDate},
		{"volcengine YYMMDD", "deepseek-v3-1", "deepseek-v3-1-250821", VerdictSnapshotDate},
		{"google MM-DD", "gemini-2.5-flash-preview", "gemini-2.5-flash-preview-05-20", VerdictSnapshotDate},
		// A1 counter-examples: numbers that are not dates
		{"extra version segment", "deepseek-v3", "deepseek-v3-1-250821", VerdictMismatch},
		{"day 34", "gpt-5", "gpt-5-1234", VerdictMismatch},
		{"month 13", "gpt-5", "gpt-5-1301", VerdictMismatch},
		{"month 00", "gpt-5", "gpt-5-0010", VerdictMismatch},
		{"iso month 13", "gpt-5", "gpt-5-2025-13-01", VerdictMismatch},
		{"compact day 40", "gpt-5", "gpt-5-20251340", VerdictMismatch},
		{"nine digits", "gpt-5", "gpt-5-123456789", VerdictMismatch},
		// A1 direction: pinned build sent, bare alias returned
		{"pinned openai", "gpt-5-2025-08-07", "gpt-5", VerdictPinnedToAlias},
		{"pinned anthropic", "claude-haiku-4-5-20251001", "claude-haiku-4-5", VerdictPinnedToAlias},
		// A2 -latest must not hide a lower tier
		{"latest same tier", "gemini-flash-latest", "gemini-3.7-flash", VerdictRollingAlias},
		{"latest pro resolves pro", "gemini-pro-latest", "gemini-3.1-pro", VerdictRollingAlias},
		{"latest flash to flash-lite", "gemini-flash-latest", "gemini-3.7-flash-lite", VerdictMismatch},
		{"latest pro to preview", "gemini-pro-latest", "gemini-3.1-pro-preview", VerdictRollingAlias},
		{"latest flash to dated preview", "gemini-flash-latest", "gemini-2.5-flash-preview-09-2025", VerdictRollingAlias},
		{"latest flash to lite preview", "gemini-flash-latest", "gemini-2.5-flash-lite-preview-09-2025", VerdictMismatch},
		{"preview exemption is gemini-only", "gpt-latest", "gpt-5-preview", VerdictMismatch},
		{"google MM-YYYY", "gemini-2.5-flash-preview", "gemini-2.5-flash-preview-09-2025", VerdictSnapshotDate},
		{"MM-YYYY month 13", "gemini-2.5-flash-preview", "gemini-2.5-flash-preview-13-2025", VerdictMismatch},
		// Vertex @YYYYMMDD pins on the wire vs the Claude body's model
		{"vertex at-date", "claude-sonnet-4-5@20250929", "claude-sonnet-4-5-20250929", VerdictVendorPrefix},
		{"vertex v2 at-date", "claude-3-5-sonnet-v2@20241022", "claude-3-5-sonnet-20241022", VerdictVendorPrefix},
		{"vertex pin different build", "claude-sonnet-4-5@20250929", "claude-sonnet-4-5-20250514", VerdictMismatch},
		{"vertex pin answered by alias", "claude-sonnet-4-5@20250929", "claude-sonnet-4-5", VerdictPinnedToAlias},
		{"latest suffix form", "claude-sonnet-4-6", "claude-sonnet-4-6-latest", VerdictRollingAlias},
		// A3 Bedrock build suffix
		{"both builds differ", "anthropic.claude-haiku-4-5-20251001-v1:0", "anthropic.claude-haiku-4-5-20251001-v2:0", VerdictMismatch},
		{"both builds equal", "anthropic.claude-haiku-4-5-20251001-v1:0", "us.anthropic.claude-haiku-4-5-20251001-v1:0", VerdictVendorPrefix},
		{"wire id vs body model", "us.anthropic.claude-sonnet-4-5-20250929-v1:0", "claude-sonnet-4-5-20250929", VerdictVendorPrefix},
		{"bare sent, bedrock build back", "claude-sonnet-4-5", "global.anthropic.claude-sonnet-4-5-v1:0", VerdictVendorPrefix},
		{"-vN kept on non-bedrock ids", "deepseek-v3", "deepseek-v3", VerdictExact},
		{"non-bedrock -vN differs", "deepseek-v3", "deepseek-v2", VerdictMismatch},
		// A5 Gemini -NNN build, alias → build only, gemini family only
		{"gemini build", "gemini-2.0-flash", "gemini-2.0-flash-001", VerdictVersionSuffix},
		{"gemini build reversed", "gemini-2.0-flash-001", "gemini-2.0-flash", VerdictPinnedToAlias},
		{"non-gemini -NNN", "gpt-4o", "gpt-4o-001", VerdictMismatch},
		{"gemini two digits", "gemini-2.0-flash", "gemini-2.0-flash-01", VerdictMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyUpstreamModelMismatch(c.sent, c.resp)
			assert.Equal(t, c.want, got)
			assert.Equal(t, VerdictIsMismatch(c.want), !UpstreamModelsMatchForAudit(c.sent, c.resp))
		})
	}
}

// TestCanonicalModelPairRecordsPrefix pins spec A4: the prefix is stripped for
// the comparison but reported so the tracker and the log can see it.
func TestCanonicalModelPairRecordsPrefix(t *testing.T) {
	cases := []struct{ sent, resp, wantSent, wantResp, wantPrefix string }{
		{"gpt-5", "openai/gpt-5", "gpt-5", "gpt-5", "openai"},
		{"claude-sonnet-5", "global.anthropic.claude-sonnet-5", "claude-sonnet-5", "claude-sonnet-5", "global.anthropic"},
		{"deepseek-v4-flash", "deepseek-v4-flash", "deepseek-v4-flash", "deepseek-v4-flash", ""},
		{"x", "openrouter/deepseek/deepseek-v4", "x", "deepseek-v4", "openrouter/deepseek"},
	}
	for _, c := range cases {
		sent, resp, prefix := canonicalModelPair(c.sent, c.resp)
		assert.Equal(t, c.wantSent, sent, c.resp)
		assert.Equal(t, c.wantResp, resp, c.resp)
		assert.Equal(t, c.wantPrefix, prefix, c.resp)
	}
}

// TestTierAndVersionDowngradeReplacementForms pins spec A6: replacement forms,
// responses that lost the family word (#57), "turbo" is not a downgrade, and
// the version flag is independent of the tier flag.
func TestTierAndVersionDowngradeReplacementForms(t *testing.T) {
	cases := []struct {
		sent, resp    string
		tier, version bool
	}{
		// the #57 production case: no gemini- prefix, lower tier AND older version
		{"gemini-3.7-flash", "3.1-flash-lite-preview-03-2026", true, true},
		{"gemini-2.5-pro", "gemini-2.5-flash", true, false},
		{"gemini-2.5-pro", "gemini-2.5-flash-lite", true, false},
		{"gemini-2.5-flash", "gemini-2.5-flash-lite", true, false},
		{"claude-opus-4-7", "claude-sonnet-4-7", true, false},
		{"claude-opus-4-7", "claude-haiku-4-5", true, true},
		{"claude-sonnet-4-6", "claude-haiku-4-5", true, true},
		{"gpt-4o", "gpt-4o-mini", true, false},
		{"qwen-max", "qwen-plus", true, false},
		{"claude-opus-4-7", "claude-opus-4-5-20251101", false, true},
		{"deepseek-v4", "deepseek-v3", false, true},
		{"gpt-5", "gpt-4o", false, true},
		// negatives
		{"gpt-4", "gpt-4-turbo", false, false},
		{"qwen-plus", "qwen-max", false, false},
		{"claude-sonnet-4-6", "claude-opus-4-7", false, false},
		{"claude-haiku-4-5", "claude-sonnet-4-6", false, false},
		{"gemini-2.5-flash", "gemini-2.5-pro", false, false},
		{"claude-opus-4-5", "claude-opus-4-7", false, false},
		{"gpt-5.5", "gpt-5-6-luna", false, false},
		{"claude-3-5-sonnet", "claude-sonnet-4-5", false, false},
		{"claude-opus-4-7", "gpt-4o-mini", false, false}, // different family: not a sibling
		{"claude-opus-4-7", "deepseek-v3", false, false},
		{"gemini-3.7-flash", "3.7-flash", false, false},
		// not a mismatch at all: flags never apply
		{"gpt-5", "gpt-5-2025-08-07", false, false},
		{"gemini-2.0-flash", "gemini-2.0-flash-001", false, false},
	}
	for _, c := range cases {
		assert.Equal(t, c.tier, UpstreamModelIsTierDowngrade(c.sent, c.resp), "tier %s -> %s", c.sent, c.resp)
		assert.Equal(t, c.version, UpstreamModelIsVersionDowngrade(c.sent, c.resp), "version %s -> %s", c.sent, c.resp)
	}
}

func TestInferUpstreamSource(t *testing.T) {
	cases := []struct {
		name        string
		channelType int
		headers     string
		id, model   string
		want        string
	}{
		{"aws by channel type", 33, "", "chatcmpl-x", "claude-x", "aws"},
		{"vertex by channel type", 41, "", "", "gemini-x", "vertex"},
		{"azure by channel type", 3, "", "chatcmpl-x", "gpt-4", "azure"},
		{"azure by apim header", 1, `{"Apim-Request-Id":"x"}`, "chatcmpl-x", "gpt-4", "azure"},
		{"aws by amzn header", 1, `{"X-Amzn-Requestid":"x"}`, "msg_x", "claude-x", "aws"},
		{"openai official", 1, `{"Openai-Organization":"org-x"}`, "chatcmpl-x", "gpt-5", "openai"},
		{"ccmax unified ratelimit", 1, `{"Anthropic-Ratelimit-Unified-Status":"ok"}`, "msg_01x", "claude-x", "ccmax"},
		{"anthropic official", 1, `{"Anthropic-Ratelimit-Requests-Limit":"50"}`, "msg_01x", "claude-x", "anthropic"},
		{"kimi msh header", 1, `{"Msh-Request-Id":"x"}`, "chatcmpl-x", "kimi-k3", "kimi"},
		{"newapi relay", 1, `{"X-New-Api-Version":"1"}`, "chatcmpl-x", "gpt-5", "newapi_relay"},
		{"bedrock id shape", 1, "", "msg_bdrk_abc", "claude-x", "aws"},
		{"claude relay shape", 1, "", "msg_01abc", "claude-x", "claude_relay"},
		{"gpt relay shape", 1, "", "chatcmpl-abc", "gpt-5", "gpt_relay"},
		{"responses api relay shape", 1, "", "resp_0e7981fdbb93b145", "gpt-5.5", "gpt_relay"},
		{"responses api openai official", 1, `{"Openai-Processing-Ms":"123"}`, "resp_0e7981", "gpt-5.5", "openai"},
		{"deepseek uuid", 1, "", "c53e7f1e-873b-41ee-bc8b-810a0d033691", "deepseek-v4-flash", "deepseek"},
		{"deepseek chatcmpl relay", 1, "", "chatcmpl-abc", "deepseek-v4-pro", "deepseek"},
		{"glm numeric id", 1, "", "20260901123456789", "glm-5", "glm"},
		{"unknown", 1, "", "weird-123", "mystery", "unknown"},
		{"bedrock model naming behind relay", 1, "", "msg_01x", "global.anthropic.claude-sonnet-5", "aws"},
		{"vertex claude id shape", 1, "", "msg_vrtx_01abc", "claude-x", "vertex"},
		{"empty everything", 1, "", "", "", "unknown"},
	}
	for _, c := range cases {
		if got := InferUpstreamSource(c.channelType, c.headers, c.id, c.model, false); got != c.want {
			t.Errorf("%s: InferUpstreamSource = %q, want %q", c.name, got, c.want)
		}
	}
}

// ccmax hint: anthropic shape + injection evidence (2026-09-04 #119/#127 ccgateapi real traffic)
func TestInferUpstreamSourceCcmaxHint(t *testing.T) {
	cases := []struct {
		name        string
		channelType int
		headers     string
		id, model   string
		want        string
	}{
		{"ccmax via injection hint", 1, "", "msg_011CefvT", "claude-fable-5-1", "ccmax"},
		{"hint ignored for non-anthropic shape", 1, "", "chatcmpl-abc", "gpt-5", "gpt_relay"},
		{"hint never overrides official header", 1, `{"Anthropic-Organization-Id":"org"}`, "msg_01x", "claude-x", "anthropic"},
		{"hint never overrides bedrock id shape", 1, "", "msg_bdrk_abc", "claude-x", "aws"},
	}
	for _, c := range cases {
		if got := InferUpstreamSource(c.channelType, c.headers, c.id, c.model, true); got != c.want {
			t.Errorf("%s: InferUpstreamSource(ccmaxHint) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestInjectionDetected(t *testing.T) {
	cases := []struct {
		est, injected int
		want          bool
	}{
		{2, 3550, true},      // real ccgateapi sample: 2-token request, 3550 cached preamble
		{9, 3175, true},      // real sample
		{100, 1500, false},   // below absolute floor
		{30000, 6000, false}, // 20% tokenizer drift on a huge prompt must not trip
		{30000, 20000, true}, // genuinely large injection on a large prompt
		{0, 5000, false},     // no estimate → cannot judge
	}
	for _, c := range cases {
		if got := injectionDetected(c.est, c.injected); got != c.want {
			t.Errorf("injectionDetected(%d,%d) = %v, want %v", c.est, c.injected, got, c.want)
		}
	}
}

func TestSourceFlipFamily(t *testing.T) {
	ch := isolatedAuditKey(t, 987655, "claude")
	trackSourceFlip(ch, "claude_relay")
	if _, flipped := trackSourceFlip(ch, "ccmax"); flipped {
		t.Error("claude_relay -> ccmax is the same relay, must not count as a flip")
	}
	if _, flipped := trackSourceFlip(ch, "claude_relay"); flipped {
		t.Error("ccmax -> claude_relay must not count as a flip either")
	}
	if from, flipped := trackSourceFlip(ch, "gpt_relay"); !flipped || from != "claude_relay" {
		t.Errorf("claude_relay -> gpt_relay must flip, got from=%q flipped=%v", from, flipped)
	}
}

func TestTrackSourceFlip(t *testing.T) {
	ch := isolatedAuditKey(t, 987654, "gpt")
	// first observation seeds silently
	if _, flipped := trackSourceFlip(ch, "azure"); flipped {
		t.Error("first observation must not flip")
	}
	// same source: no flip
	if _, flipped := trackSourceFlip(ch, "azure"); flipped {
		t.Error("same source must not flip")
	}
	// flip azure -> gpt_relay
	from, flipped := trackSourceFlip(ch, "gpt_relay")
	if !flipped || from != "azure" {
		t.Errorf("expected flip from azure, got from=%q flipped=%v", from, flipped)
	}
	// flip back gpt_relay -> azure
	from, flipped = trackSourceFlip(ch, "azure")
	if !flipped || from != "gpt_relay" {
		t.Errorf("expected flip back, got from=%q flipped=%v", from, flipped)
	}
	// repeat flip azure -> gpt_relay: no rate limiting — every flip reported
	from, flipped = trackSourceFlip(ch, "gpt_relay")
	if !flipped || from != "azure" {
		t.Errorf("expected repeat flip reported, got from=%q flipped=%v", from, flipped)
	}
	// unknown never participates
	if _, flipped := trackSourceFlip(ch, "unknown"); flipped {
		t.Error("unknown must not flip")
	}
}

// Bedrock model ids carry region/vendor prefixes and a -v1:0 suffix; they must
// canonicalize to the bare model before the mismatch verdict (2026-09-06 real
// false positives on channels #77/#100).
func TestUpstreamModelsMatchBedrockNaming(t *testing.T) {
	cases := []struct {
		sent, resp string
		want       bool
	}{
		{"claude-sonnet-5", "global.anthropic.claude-sonnet-5", true},
		{"claude-opus-4-6", "global.anthropic.claude-opus-4-6-v1", true},
		{"claude-haiku-4-5-20251001", "anthropic.claude-haiku-4-5-20251001-v1:0", true},
		{"claude-haiku-4-5", "us.anthropic.claude-haiku-4-5-20251001-v1:0", true}, // date snapshot rule still applies
		{"openai.gpt-5.4", "gpt-5.4", true},                                       // third-party vendor-dot alias
		{"claude-opus-4-6", "global.anthropic.claude-opus-4-7", false},            // real swap must still flag
		{"gpt-5.6-sol", "gpt-5.6-sol", true},                                      // dotted names untouched
	}
	for _, c := range cases {
		if got := UpstreamModelsMatchForAudit(c.sent, c.resp); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.sent, c.resp, got, c.want)
		}
	}
}

func TestInferBackendShapeLooksThroughRelayHeaders(t *testing.T) {
	// hop label says relay, backend shape says Bedrock — both must be reported
	hdr := `{"X-New-Api-Version":"v1.0.0-rc.20"}`
	if got := InferUpstreamSource(14, hdr, "msg_bdrk_abc", "global.anthropic.claude-sonnet-5", false); got != "newapi_relay" {
		t.Fatalf("hop label = %q, want newapi_relay", got)
	}
	backend, by := inferBackendShape(14, "msg_bdrk_abc", "global.anthropic.claude-sonnet-5", false)
	assert.Equal(t, "aws", backend)
	assert.Equal(t, BackendByIdShape, by)
	backend, by = inferBackendShape(14, "msg_01abc", "claude-sonnet-5", false)
	assert.Equal(t, "claude_relay", backend)
	assert.Equal(t, BackendByFallback, by)
}

func TestTrackBackendFlipIsIndependentOfSource(t *testing.T) {
	ch := isolatedAuditKey(t, 987653, "claude")
	assert.Empty(t, trackBackendFlip(ch, "aws", BackendByIdShape, nil).flip, "seed must not flip")
	if _, f := trackSourceFlip(ch, "newapi_relay"); f {
		t.Fatal("source seed must not flip")
	}
	// msg_bdrk_ → msg_: the id shape is on every answer, so its change is real
	assert.Equal(t, "aws->claude_relay", trackBackendFlip(ch, "claude_relay", BackendByFallback, nil).flip)
	if _, f := trackSourceFlip(ch, "newapi_relay"); f {
		t.Fatal("source unchanged must not flip when only backend moved")
	}
}

// The error-log path calls the exported wrapper on every recorded error,
// including ones where the request never reached an upstream. A nil relayInfo
// leaves the log untouched; an unobserved exchange is recorded as obs=none
// (so "not observed" is visible) without any verdict.
func TestAppendUpstreamResponseAuditWithoutObservation(t *testing.T) {
	other := model.NewLogOther()
	AppendUpstreamResponseAuditForError(nil, nil, other, "upstream_error")
	_, ok := other.Snapshot()["root_info"]
	require.False(t, ok, "nil relayInfo must not write anything")

	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.5"}}
	AppendUpstreamResponseAuditForError(nil, info, other, "upstream_error")
	ursp := urspOf(t, other)
	assert.Equal(t, map[string]any{"obs": "none", "h": "unobserved", "err": "upstream_error", "sent": "gpt-5.5"}, ursp)
}

// An observed error response yields a ursp block that is labelled as failed,
// still carries the mismatch verdict, and does not feed the flip trackers
// (an error answered with the relay's own headers must not look like a
// provider switch).
func TestAppendUpstreamResponseAuditForErrorLabelsAndSkipsFlip(t *testing.T) {
	const ch = 987652
	isolatedAuditKey(t, ch, "gpt")
	seed := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.5", ChannelId: ch}}
	seed.ObserveUpstreamResponse("gpt-5.5", "chatcmpl-ok", 1, "")
	seed.SetUpstreamResponseForensics(`{"id":"chatcmpl-ok"}`, map[string][]string{"Openai-Organization": {"org-x"}})
	appendUpstreamResponseAudit(nil, seed, model.NewLogOther(), "") // seeds src=openai for the channel

	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.5", ChannelId: ch}}
	info.ObserveUpstreamResponse("gpt-5-6-luna", "chatcmpl-err", 2, "")
	info.SetUpstreamResponseForensics(`{"error":{"type":"server_error"}}`, map[string][]string{"X-New-Api-Version": {"v1"}})
	other := model.NewLogOther()
	AppendUpstreamResponseAuditForError(nil, info, other, "upstream_error")

	root, _ := other.Snapshot()["root_info"].(map[string]interface{})
	ursp, ok := root["ursp"].(map[string]interface{})
	if !ok {
		t.Fatalf("ursp missing on error path: %+v", other.Snapshot())
	}
	if ursp["err"] != "upstream_error" {
		t.Fatalf("err label missing: %+v", ursp)
	}
	if ursp["mismatch"] != true || ursp["kind"] != VerdictMismatch {
		t.Fatalf("verdict not applied on error path: %+v", ursp)
	}
	if _, flipped := ursp["src_flip"]; flipped {
		t.Fatalf("error-shaped response fed the source flip tracker: %+v", ursp)
	}
	// and the tracker state is untouched: a following clean success must not flip either
	after := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.5", ChannelId: ch}}
	after.ObserveUpstreamResponse("gpt-5.5", "chatcmpl-ok2", 3, "")
	after.SetUpstreamResponseForensics(`{"id":"chatcmpl-ok2"}`, map[string][]string{"Openai-Organization": {"org-x"}})
	otherAfter := model.NewLogOther()
	appendUpstreamResponseAudit(nil, after, otherAfter, "")
	rootAfter, _ := otherAfter.Snapshot()["root_info"].(map[string]interface{})
	urspAfter, _ := rootAfter["ursp"].(map[string]interface{})
	if _, flipped := urspAfter["src_flip"]; flipped {
		t.Fatalf("success after error flipped, so the error must have moved the tracker: %+v", urspAfter)
	}
}

// decideUpstreamBackend: channel type (strong) > official headers (medium) >
// body fingerprints (medium) > positive id/model shape (weak) > neutral
// protocol-format fallback (none); disagreement is recorded, never hidden.
func TestDecideUpstreamBackend(t *testing.T) {
	azureBody := relaycommon.BodyMarkerAzureContentFilter
	deepseekBody := relaycommon.BodyMarkerDeepSeekCacheHit | relaycommon.BodyMarkerDeepSeekCacheMiss
	cases := []struct {
		name         string
		channelType  int
		headerSource string
		markers      relaycommon.UpstreamBodyMarkers
		id, model    string
		want         backendDecision
	}{
		{"azure channel type", constant.ChannelTypeAzure, "", 0, "chatcmpl-a1", "gpt-4o",
			backendDecision{backend: "azure", by: BackendByChannelType, conf: BackendConfStrong}},
		{"official azure headers", 1, "azure", 0, "chatcmpl-a1", "gpt-4o",
			backendDecision{backend: "azure", by: BackendByHeader, conf: BackendConfMedium}},
		{"relay headers + azure body", 1, "newapi_relay", azureBody, "resp_a1", "gpt-5.1",
			backendDecision{backend: "azure", by: BackendByBody, conf: BackendConfMedium}},
		{"bedrock metrics without msg_bdrk_", 14, "", relaycommon.BodyMarkerBedrockMetrics, "msg_01abc", "claude-sonnet-5",
			backendDecision{backend: "aws", by: BackendByBody, conf: BackendConfMedium}},
		{"deepseek usage fields", 1, "", deepseekBody, "chatcmpl-a1", "deepseek-v4",
			backendDecision{backend: "deepseek", by: BackendByBody, conf: BackendConfMedium}},
		{"header openai vs body azure", 1, "openai", azureBody, "chatcmpl-a1", "gpt-4o",
			backendDecision{backend: "openai", by: BackendByHeader, conf: BackendConfMedium,
				conflict: []string{"openai:header", "azure:body"}}},
		{"azure headers vs bedrock id", 1, "azure", 0, "msg_bdrk_01abc", "claude-sonnet-5",
			backendDecision{backend: "azure", by: BackendByHeader, conf: BackendConfMedium,
				conflict: []string{"azure:header", "aws:id_shape"}}},
		{"openrouter id", 1, "", 0, "gen-123", "gpt-5",
			backendDecision{backend: "openrouter", by: BackendByIdShape, conf: BackendConfWeak}},
		{"glm naming", 1, "", 0, "chatcmpl-a1", "glm-4.6",
			backendDecision{backend: "glm", by: BackendByModelName, conf: BackendConfWeak}},
		{"plain openai-shaped relay", 1, "newapi_relay", 0, "chatcmpl-a1", "gpt-5",
			backendDecision{backend: "gpt_relay", by: BackendByFallback, conf: BackendConfNone}},
		{"plain anthropic-shaped relay", 14, "", 0, "msg_01abc", "claude-sonnet-5",
			backendDecision{backend: "claude_relay", by: BackendByFallback, conf: BackendConfNone}},
		{"relay hop is never a backend", 1, "newapi_relay", relaycommon.BodyMarkerNewAPICacheSplit, "", "",
			backendDecision{backend: "unknown", by: BackendByFallback, conf: BackendConfNone}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideUpstreamBackend(c.channelType, c.headerSource, c.markers, c.id, c.model, false)
			if len(c.want.conflict) == 0 {
				c.want.conflict = nil
			}
			assert.Equal(t, c.want, got)
		})
	}
}

// isolatedAuditKey returns the tracker key for a test channel and removes that
// channel from every global audit tracker (flip trackers, ccmax stickiness,
// replay memory) when the test ends, so shuffled runs cannot leak state.
func isolatedAuditKey(t *testing.T, channelId int, family string) flipTrackerKey {
	t.Helper()
	forget := func() {
		for _, tracker := range []*sync.Map{&srcTracker, &backendTracker} {
			tracker.Range(func(k, _ any) bool {
				if k.(flipTrackerKey).channelId == channelId {
					tracker.Delete(k)
				}
				return true
			})
		}
		ccmaxChannels.Delete(channelId)
		constantCreated.channels.Delete(channelId)
		replayMemory.mu.Lock()
		marker := "|" + strconv.Itoa(channelId) + "|"
		for key, el := range replayMemory.entries {
			if strings.Contains(key, marker) {
				replayMemory.order.Remove(el)
				delete(replayMemory.entries, key)
			}
		}
		replayMemory.mu.Unlock()
	}
	forget()
	t.Cleanup(forget)
	return flipTrackerKey{channelId: channelId, family: family}
}

// urspOf extracts root_info.ursp from a log.
func urspOf(t *testing.T, other *model.LogOther) map[string]any {
	t.Helper()
	root, _ := other.Snapshot()["root_info"].(map[string]any)
	ursp, ok := root["ursp"].(map[string]any)
	require.True(t, ok, "ursp missing: %+v", other.Snapshot())
	return ursp
}

// auditExchange describes one observed upstream exchange for the audit tests.
type auditExchange struct {
	channelId   int
	requestId   string
	sent        string
	models      []string // declared models, in stream order
	id          string
	created     int64
	headers     map[string][]string
	excerpt     string
	output      string
	promptHash  string
	temperature *float64
	stream      bool
	channelTest bool
	keyIndex    int
	multiKey    bool
	completion  int
	estimate    int
	elapsed     time.Duration
	startedAt   time.Time // request start; defaults to now − elapsed
	handler     string
	useChannels []string
	errorType   string
}

// runAuditExchange feeds one exchange through appendUpstreamResponseAudit and
// returns its ursp block and the evidence row it queued (nil if none).
func runAuditExchange(t *testing.T, x auditExchange) (map[string]any, *model.UpstreamEvidence) {
	t.Helper()
	var queued *model.UpstreamEvidence
	original := enqueueUpstreamEvidence
	enqueueUpstreamEvidence = func(e *model.UpstreamEvidence) bool { queued = e; return true }
	t.Cleanup(func() { enqueueUpstreamEvidence = original })

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set(common.RequestIdKey, x.requestId)
	if x.useChannels != nil {
		ctx.Set("use_channel", x.useChannels)
	}
	if x.sent == "" {
		x.sent = "gpt-5"
	}
	if x.handler == "" {
		x.handler = "openai_chat"
	}
	if x.elapsed == 0 {
		x.elapsed = 10 * time.Second
	}
	info := &relaycommon.RelayInfo{
		StartTime:     time.Now().Add(-x.elapsed),
		IsStream:      x.stream,
		IsChannelTest: x.channelTest,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: x.sent, ChannelId: x.channelId, ChannelType: 1,
			ChannelIsMultiKey: x.multiKey, ChannelMultiKeyIndex: x.keyIndex,
		},
	}
	if !x.startedAt.IsZero() {
		info.StartTime = x.startedAt
	}
	info.SetEstimatePromptTokens(x.estimate)
	info.MarkUpstreamObserver(x.handler)
	info.SnapshotUpstreamWire(x.sent, nil)
	info.UpstreamAudit.PromptSHA256 = x.promptHash
	info.UpstreamAudit.Temperature = x.temperature
	for _, m := range x.models {
		info.ObserveUpstreamResponseFrame(`{"model":"`+m+`"}`, m, x.id, x.created, "")
	}
	if len(x.models) == 0 {
		info.ObserveUpstreamResponse("", x.id, x.created, "")
	}
	excerpt := x.excerpt
	if excerpt == "" {
		excerpt = `{"id":"` + x.id + `","object":"chat.completion","model":"x","choices":[]}`
	}
	info.SetUpstreamResponseForensics(excerpt, x.headers)
	info.ObserveUpstreamOutput(x.output)
	info.ObserveUpstreamCompletionTokens(x.completion)
	other := model.NewLogOther()
	appendUpstreamResponseAudit(ctx, info, other, x.errorType)
	return urspOf(t, other), queued
}

var longAnswer = strings.Repeat("The quick brown fox jumps over the lazy dog. ", 3)

func TestAuditHappyPathCarriesNoSignals(t *testing.T) {
	isolatedAuditKey(t, 987656, "gpt")
	ursp, ev := runAuditExchange(t, auditExchange{
		channelId: 987656, requestId: "req-happy", models: []string{"gpt-5-2025-08-07"},
		id: "chatcmpl-happy0001", created: time.Now().Unix(), output: longAnswer,
		promptHash: strings.Repeat("a", 64), completion: 150, stream: true,
	})
	for _, flag := range []string{"stale_created", "dup_id", "dup_output", "replay", "implausible_speed",
		"extra_keys", "mid_stream_change", "tier_downgrade", "version_downgrade", "src_flip", "backend_flip",
		"sent_field", "retry", "err"} {
		assert.NotContains(t, ursp, flag)
	}
	assert.Equal(t, "full", ursp["obs"])
	assert.Equal(t, "openai_chat", ursp["h"])
	assert.Equal(t, VerdictSnapshotDate, ursp["kind"])
	require.NotNil(t, ev)
	assert.Equal(t, "normal", ev.Kind)
	assert.Empty(t, ev.Note)
	assert.Equal(t, strings.Repeat("a", 64), ev.PromptSha256)
	assert.Len(t, ev.OutputSha256, 64)
	assert.Equal(t, `{"model":"gpt-5-2025-08-07"}`, ev.ModelFrame)
}

func TestAuditReplaySignals(t *testing.T) {
	const ch = 987657
	isolatedAuditKey(t, ch, "gpt")
	now := time.Now().Unix()
	promptA, promptB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	warm := 0.7
	answerX := strings.Repeat("The cached answer keeps coming back word for word. ", 5)
	answerY := strings.Repeat("A first distinctive answer that only appears once here. ", 5)
	answerZ := strings.Repeat("A second distinctive answer, also produced exactly once. ", 5)
	run := func(req, id, output, prompt string, created int64, temp *float64) (map[string]any, *model.UpstreamEvidence) {
		return runAuditExchange(t, auditExchange{
			channelId: ch, requestId: req, models: []string{"gpt-5"}, id: id, created: created,
			output: output, promptHash: prompt, temperature: temp,
		})
	}

	ursp, _ := run("req-a", "chatcmpl-idone0001", answerX, promptA, now, nil)
	assert.NotContains(t, ursp, "dup_output")

	// same long output for another prompt under sampling: reasons are recorded
	// but a duplicate output alone is only a flag
	ursp, ev := run("req-b", "chatcmpl-idtwo0001", answerX, promptB, now, &warm)
	assert.Equal(t, true, ursp["dup_output"])
	assert.NotContains(t, ursp, "replay")
	assert.Equal(t, "normal", ev.Kind)
	assert.Equal(t, "signals: dup_output,prompt_differs,sampled", ev.Note)

	// the same id AND the same long output as an earlier request (req-a; the
	// output's latest owner is req-b, so this is the id branch) for a
	// different prompt: replay
	ursp, ev = run("req-c", "chatcmpl-idone0001", answerX, promptB, now, nil)
	assert.Equal(t, true, ursp["dup_id"])
	assert.Equal(t, true, ursp["replay"])
	assert.Equal(t, "replay_suspect", ev.Kind)
	assert.Contains(t, ev.Note, "signals: dup_id,dup_output")

	// a retry of the same request repeating its id is not a duplicate
	ursp, _ = run("req-c", "chatcmpl-idone0001", answerX, promptB, now, nil)
	assert.NotContains(t, ursp, "dup_id")

	// a relay with a fixed id: text, then a tool-call-only turn (no output
	// hash), then fresh text — only dup_id flags, and the empty turn does
	// not poison the comparison for the next one
	run("req-f1", "msg_01fixedidfixed", answerY, promptA, now, nil)
	ursp, ev = run("req-f2", "msg_01fixedidfixed", "", promptB, now, nil)
	assert.Equal(t, true, ursp["dup_id"])
	assert.NotContains(t, ursp, "replay")
	assert.Equal(t, "normal", ev.Kind)
	ursp, _ = run("req-f3", "msg_01fixedidfixed", answerZ, promptB, now, nil)
	assert.Equal(t, true, ursp["dup_id"])
	assert.NotContains(t, ursp, "replay")

	// handlers that never compute an output hash cannot produce a replay
	run("req-h1", "as-12345678-legacy", "", promptA, now, nil)
	ursp, _ = run("req-h2", "as-12345678-legacy", "", promptB, now, nil)
	assert.Equal(t, true, ursp["dup_id"])
	assert.NotContains(t, ursp, "replay")

	// constant-id upstream + a deterministic short answer with fresh, distinct
	// created stamps: only dup_id (spec review M-1)
	run("req-k1", "chatcmpl-const0001", "OK", promptA, now-5, nil)
	ursp, ev = run("req-k2", "chatcmpl-const0001", "OK", promptB, now, nil)
	assert.Equal(t, true, ursp["dup_id"])
	assert.NotContains(t, ursp, "replay")
	assert.Equal(t, "normal", ev.Kind)

	// same id + same short text + the very same FRESH created (two answers in
	// one second behind a constant-id upstream): only dup_id (review round 4)
	greeting := "Hello! How can I help you with it today?" // 40 chars
	run("req-g1", "chatcmpl-greet0001", greeting, promptA, now-3, nil)
	ursp, ev = run("req-g2", "chatcmpl-greet0001", greeting, promptB, now-3, nil)
	assert.Equal(t, true, ursp["dup_id"])
	assert.NotContains(t, ursp, "replay")
	assert.Equal(t, "normal", ev.Kind)

	// constant-id upstream + the same prompt + a deterministic long answer
	// with a fresh created: an honest repeated request, only dup_id
	answerD := strings.Repeat("A deterministic answer to an identical prompt, repeated honestly. ", 4)
	run("req-d1", "chatcmpl-constlong01", answerD, promptA, now-2, nil)
	ursp, ev = run("req-d2", "chatcmpl-constlong01", answerD, promptA, now, nil)
	assert.Equal(t, true, ursp["dup_id"])
	assert.NotContains(t, ursp, "replay")
	assert.Equal(t, "normal", ev.Kind)

	// same id + a different short text: a constant-id generator, dup_id only
	ursp, _ = run("req-g3", "chatcmpl-greet0001", "Sure, what would you like to change?", promptA, now-3, nil)
	assert.Equal(t, true, ursp["dup_id"])
	assert.NotContains(t, ursp, "replay")

	// same id + a tool-call-only turn (no text hash): dup_id only
	ursp, _ = run("req-g4", "chatcmpl-greet0001", "", promptB, now-3, nil)
	assert.Equal(t, true, ursp["dup_id"])
	assert.NotContains(t, ursp, "replay")

	// the #148 end-to-end shape: same id, same 22-day-old created, same short
	// answer for two different prompts
	stale := now - 22*86400
	run("req-c1", "chatcmpl-cache0148", greeting+" (cached)", promptA, stale, nil)
	ursp, ev = run("req-c2", "chatcmpl-cache0148", greeting+" (cached)", promptB, stale, nil)
	assert.Equal(t, true, ursp["replay"])
	assert.Equal(t, "replay_suspect", ev.Kind)
	assert.True(t, strings.HasPrefix(ev.Note, "signals: dup_id,stale_created"), ev.Note)

	// the same long output under another id with a stale stamp that is NOT
	// the earlier answer's stamp: a skewed backend, not a cache (a cache
	// hands back the original stamp; see TestAuditRepeatedCacheReplaysAllCount)
	ursp, ev = run("req-s", "chatcmpl-stale0001", answerX, promptB, now-23*86400, nil)
	assert.Equal(t, true, ursp["stale_created"])
	assert.Equal(t, true, ursp["dup_output"])
	assert.NotContains(t, ursp, "replay")
	assert.Equal(t, "normal", ev.Kind)

	// stale created alone is only a flag (a constant created on fresh ids)
	ursp, ev = run("req-t", "chatcmpl-seven0001", strings.Repeat("unique ", 40), promptA, now-86400, nil)
	assert.Equal(t, true, ursp["stale_created"])
	assert.NotContains(t, ursp, "replay")
	assert.Equal(t, "normal", ev.Kind)

	// millisecond stamps are normalised, not stale
	ursp, _ = run("req-m", "chatcmpl-eight0001", "short", promptA, time.Now().UnixMilli(), nil)
	assert.NotContains(t, ursp, "stale_created")

	// placeholder ids never count as duplicates
	run("req-p1", "resp_0", "ok", promptA, now, nil)
	ursp, _ = run("req-p2", "resp_0", "ok", promptB, now, nil)
	assert.NotContains(t, ursp, "dup_id")
	assert.NotContains(t, ursp, "dup_output")
}

// Channel tests may consult the replay memory but never add to it, never feed
// the flip trackers, and are labelled in the evidence note (spec F2).
func TestAuditChannelTestDoesNotFeedTrackers(t *testing.T) {
	const ch = 987658
	isolatedAuditKey(t, ch, "gpt")
	azure := map[string][]string{"Apim-Request-Id": {"x"}}
	relay := map[string][]string{"X-New-Api-Version": {"v1"}}
	_, ev := runAuditExchange(t, auditExchange{channelId: ch, requestId: "t-1", models: []string{"gpt-5"},
		id: "chatcmpl-test00001", headers: azure, channelTest: true})
	assert.Equal(t, "channel_test", ev.Note)
	ursp, _ := runAuditExchange(t, auditExchange{channelId: ch, requestId: "t-2", models: []string{"gpt-5"},
		id: "chatcmpl-test00002", headers: relay, channelTest: true})
	assert.NotContains(t, ursp, "src_flip")

	// the real request reusing the channel test's id is not a duplicate: the
	// test never recorded it
	ursp, _ = runAuditExchange(t, auditExchange{channelId: ch, requestId: "real-1", models: []string{"gpt-5"},
		id: "chatcmpl-test00001", headers: relay})
	assert.NotContains(t, ursp, "dup_id")
	assert.NotContains(t, ursp, "src_flip", "the tracker was seeded by this request, not by the tests")
}

// Trackers are keyed by (channel, model family, key index): a multi-model or
// multi-key channel does not produce provider_switch rows across models or
// keys, while a real change on one key still flips; the vendor prefix is part
// of the backend signature (spec F1, A4).
func TestAuditTrackersKeyedByFamilyAndKey(t *testing.T) {
	const ch = 987659
	isolatedAuditKey(t, ch, "gpt")
	azure := map[string][]string{"Apim-Request-Id": {"x"}}
	relay := map[string][]string{"X-New-Api-Version": {"v1"}}

	runAuditExchange(t, auditExchange{channelId: ch, requestId: "f-1", sent: "gpt-5", models: []string{"gpt-5"}, id: "chatcmpl-fam00001", headers: azure})
	ursp, _ := runAuditExchange(t, auditExchange{channelId: ch, requestId: "f-2", sent: "claude-sonnet-5", models: []string{"claude-sonnet-5"}, id: "msg_bdrk_fam00002", headers: relay})
	assert.NotContains(t, ursp, "src_flip", "another model family is another tracker")
	assert.NotContains(t, ursp, "backend_flip")

	runAuditExchange(t, auditExchange{channelId: ch, requestId: "k-1", sent: "gpt-5", models: []string{"gpt-5"}, id: "chatcmpl-key00001", headers: azure, multiKey: true, keyIndex: 1})
	ursp, _ = runAuditExchange(t, auditExchange{channelId: ch, requestId: "k-2", sent: "gpt-5", models: []string{"gpt-5"}, id: "chatcmpl-key00002", headers: relay, multiKey: true, keyIndex: 2})
	assert.NotContains(t, ursp, "src_flip", "another key is another tracker")
	ursp, ev := runAuditExchange(t, auditExchange{channelId: ch, requestId: "k-3", sent: "gpt-5", models: []string{"gpt-5"}, id: "chatcmpl-key00003", headers: relay, multiKey: true, keyIndex: 1})
	assert.Equal(t, "azure->newapi_relay", ursp["src_flip"])
	assert.Equal(t, "provider_switch", ev.Kind)

	// a fresh key, same shape: a vendor prefix appearing or disappearing is
	// the same backend; a different vendor between two prefixed answers is not
	run := func(req, declared string) map[string]any {
		ursp, _ := runAuditExchange(t, auditExchange{channelId: ch, requestId: req, sent: "gpt-5", models: []string{declared},
			id: "chatcmpl-" + req + "-000", headers: relay, multiKey: true, keyIndex: 3})
		return ursp
	}
	ursp = run("k-4", "vendorx/gpt-5")
	assert.Equal(t, "vendorx", ursp["prefix"])
	assert.NotContains(t, ursp, "backend_flip", "prefix appearing is not a flip")
	assert.NotContains(t, run("k-5", "gpt-5"), "backend_flip", "prefix disappearing is not a flip")
	assert.Equal(t, "gpt_relay/vendorx->gpt_relay/vendory", run("k-6", "vendory/gpt-5")["backend_flip"],
		"the last vendor is remembered across an unprefixed answer")
}

// Region prefixes and Gemini's models/ path are not vendors: alternating them
// never reads as a backend flip (spec review M5).
func TestBackendSignatureComparesVendorSegmentOnly(t *testing.T) {
	cases := []struct{ shape, prefix, want string }{
		{"aws", "us.anthropic", "aws/anthropic"},
		{"aws", "global.anthropic", "aws/anthropic"},
		{"aws", "jp.anthropic", "aws/anthropic"},
		{"gpt_relay", "openrouter/deepseek", "gpt_relay/deepseek"},
		{"aistudio", "models", "aistudio"},
		{"gpt_relay", "", "gpt_relay"},
		{"unknown", "openai", "unknown"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, backendSignature(c.shape, c.prefix), c.prefix)
	}
	key := isolatedAuditKey(t, 987666, "claude")
	for _, prefix := range []string{"us.anthropic", "", "global.anthropic", "apac.anthropic"} {
		assert.Empty(t, trackBackendFlip(key, backendSignature("aws", prefix), BackendByModelName, nil).flip, prefix)
	}
}

func TestAuditMidStreamChangeIsAnAnomaly(t *testing.T) {
	isolatedAuditKey(t, 987660, "gpt")
	ursp, ev := runAuditExchange(t, auditExchange{channelId: 987660, requestId: "m-1", stream: true,
		models: []string{"gpt-5-mini", "gpt-5"}, id: "chatcmpl-mid00001"})
	assert.Equal(t, true, ursp["mid_stream_change"])
	assert.Equal(t, []string{"gpt-5-mini", "gpt-5"}, ursp["models"])
	assert.Equal(t, false, ursp["mismatch"], "the verdict still judges the final model")
	assert.Equal(t, "mid_stream_change", ev.Kind)
	assert.Equal(t, `{"model":"gpt-5-mini"}`, ev.ModelFrame, "the first model-bearing frame is kept")

	// a benign rename inside the stream (alias → dated snapshot) is listed
	// but is not a model change
	ursp, ev = runAuditExchange(t, auditExchange{channelId: 987660, requestId: "m-0", stream: true,
		models: []string{"gpt-5", "gpt-5-2025-08-07"}, id: "chatcmpl-mid00000"})
	assert.NotContains(t, ursp, "mid_stream_change")
	assert.Equal(t, []string{"gpt-5", "gpt-5-2025-08-07"}, ursp["models"])
	assert.Equal(t, "normal", ev.Kind)

	// two later builds that disagree with each other, though each is a benign
	// rename of the first alias, are a change too
	ursp, ev = runAuditExchange(t, auditExchange{channelId: 987660, requestId: "m-p", stream: true,
		models: []string{"gpt-5", "gpt-5-2025-08-07", "gpt-5-2025-09-01"}, id: "chatcmpl-midpair01"})
	assert.Equal(t, true, ursp["mid_stream_change"])
	assert.Equal(t, "mid_stream_change", ev.Kind)
	assert.Contains(t, ev.Note, "mid_stream_change: gpt-5 -> gpt-5-2025-08-07 -> gpt-5-2025-09-01")

	// combined with a mismatch the row stays a model_mismatch, noted
	ursp, ev = runAuditExchange(t, auditExchange{channelId: 987660, requestId: "m-2", stream: true,
		models: []string{"gpt-5", "gpt-5-mini"}, id: "chatcmpl-mid00002"})
	assert.Equal(t, true, ursp["mismatch"])
	assert.Equal(t, "model_mismatch", ev.Kind)
	assert.Contains(t, ev.Note, "mid_stream_change: gpt-5 -> gpt-5-mini")
}

func TestAuditSpeedAndExtraKeys(t *testing.T) {
	isolatedAuditKey(t, 987661, "gpt")
	ursp, _ := runAuditExchange(t, auditExchange{channelId: 987661, requestId: "s-1", models: []string{"gpt-5"},
		id: "chatcmpl-speed0001", completion: 2000, elapsed: time.Second, estimate: 1000,
		excerpt: `{"id":"chatcmpl-speed0001","object":"chat.completion","model":"gpt-5","choices":[],"provider":"x","api_key_id":"k"}`})
	assert.Equal(t, true, ursp["implausible_speed"])
	assert.Equal(t, []string{"api_key_id", "provider"}, ursp["extra_keys"])
	assert.NotContains(t, ursp, "pt_ratio", "the prompt-token ratio is no longer recorded")

	ursp, _ = runAuditExchange(t, auditExchange{channelId: 987661, requestId: "s-2", models: []string{"gpt-5"},
		id: "chatcmpl-speed0002", completion: 2000, elapsed: 20 * time.Second})
	assert.NotContains(t, ursp, "implausible_speed")
}

func TestAuditRecordsObservationSentFieldAndRetry(t *testing.T) {
	isolatedAuditKey(t, 987662, "gpt")
	ursp, ev := runAuditExchange(t, auditExchange{channelId: 987662, requestId: "r-1", models: []string{"gpt-5"},
		id: "chatcmpl-retry0001", useChannels: []string{"5", "987662"}})
	assert.Equal(t, 1, ursp["retry"])
	assert.Equal(t, 1, ev.RetryIndex)

	// partial: observed, but the upstream declared no model
	ursp, ev = runAuditExchange(t, auditExchange{channelId: 987662, requestId: "r-2", id: "chatcmpl-retry0002"})
	assert.Equal(t, "partial", ursp["obs"])
	assert.Equal(t, true, ursp["nomodel"])
	require.NotNil(t, ev)
}

// With digest mode on, normal rows keep only the edges of the excerpt while
// anomalies keep everything; the default stays full (spec E10).
func TestAuditDigestModeOnlyAffectsNormalRows(t *testing.T) {
	isolatedAuditKey(t, 987663, "gpt")
	big := `{"id":"chatcmpl-dig00001","model":"gpt-5","pad":"` + strings.Repeat("x", 10000) + `"}`
	require.Equal(t, audit_setting.NormalExcerptModeFull, audit_setting.NormalExcerptMode(), "default must stay full")
	_, ev := runAuditExchange(t, auditExchange{channelId: 987663, requestId: "d-0", models: []string{"gpt-5"}, id: "chatcmpl-dig00000", excerpt: big})
	assert.Equal(t, big, ev.ResponseExcerpt)

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"audit_setting.normal_excerpt_mode": "digest"}))
	t.Cleanup(func() {
		_ = config.GlobalConfig.LoadFromDB(map[string]string{"audit_setting.normal_excerpt_mode": "full"})
	})
	_, ev = runAuditExchange(t, auditExchange{channelId: 987663, requestId: "d-1", models: []string{"gpt-5"}, id: "chatcmpl-dig00001", excerpt: big})
	assert.Less(t, len(ev.ResponseExcerpt), 5000)
	assert.Contains(t, ev.ResponseExcerpt, "bytes omitted")
	assert.True(t, ev.ExcerptTruncated)
	assert.Len(t, ev.OutputSha256, 0, "no output was observed in this exchange")

	_, ev = runAuditExchange(t, auditExchange{channelId: 987663, requestId: "d-2", models: []string{"gpt-5-mini"}, id: "chatcmpl-dig00002", excerpt: big})
	assert.Equal(t, "model_mismatch", ev.Kind)
	assert.Equal(t, big, ev.ResponseExcerpt, "anomalies always keep the full excerpt")
}

// ccmax stickiness needs two visible injections within an hour and expires
// 24h after the last one (spec F3).
func TestCcmaxStickinessNeedsTwoHitsAndExpires(t *testing.T) {
	const ch = 987664
	isolatedAuditKey(t, ch, "claude")
	clock := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	original := auditNow
	auditNow = func() time.Time { return clock }
	t.Cleanup(func() { auditNow = original })

	assert.True(t, ccmaxObserve(ch, true, true), "a visible injection labels its own request")
	assert.False(t, ccmaxObserve(ch, false, true), "one hit is not enough to stick")
	clock = clock.Add(2 * time.Hour)
	assert.True(t, ccmaxObserve(ch, true, true))
	assert.False(t, ccmaxObserve(ch, false, true), "two hits two hours apart do not stick")
	clock = clock.Add(30 * time.Minute)
	ccmaxObserve(ch, true, true)
	assert.True(t, ccmaxObserve(ch, false, true), "two hits within an hour stick")
	clock = clock.Add(23 * time.Hour)
	assert.True(t, ccmaxObserve(ch, false, true))
	clock = clock.Add(2 * time.Hour)
	assert.False(t, ccmaxObserve(ch, false, true), "stickiness expires 24h after the last evidence")
	assert.True(t, ccmaxObserve(ch, true, false), "a channel test sees its own injection")
	assert.False(t, ccmaxObserve(ch, false, true), "but a channel test never feeds the state")
}

// Contract (spec G1): the text log writer keeps the upstream model name out of
// the public scope. The Claude stream path rewrites UpstreamModelName to the
// model the upstream declared, so a public copy would leak it to the user.
func TestTextLogKeepsUpstreamModelOutOfPublicScope(t *testing.T) {
	original := enqueueUpstreamEvidence
	enqueueUpstreamEvidence = func(*model.UpstreamEvidence) bool { return true }
	t.Cleanup(func() { enqueueUpstreamEvidence = original })
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/messages", nil)

	info := &relaycommon.RelayInfo{StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{
		UpstreamModelName: "claude-sonnet-5", IsModelMapped: true,
	}}
	info.SnapshotUpstreamWire("claude-sonnet-5", nil)
	info.ObserveUpstreamResponse("claude-sonnet-4-6", "msg_01leak", 0, "")
	info.UpstreamModelName = "claude-sonnet-4-6" // what the Claude stream handler does

	other := GenerateTextOtherInfo(ctx, info, 1, 1, 1, 0, 0, 0, 1)
	snapshot := other.Snapshot()
	public := make(map[string]any)
	for key, value := range snapshot {
		if key != "admin_info" && key != "root_info" && key != "audit_info" {
			public[key] = value
		}
	}
	encoded, err := common.Marshal(public)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "claude-sonnet-4-6", "the user-visible scope must not carry the declared model")
	assert.Equal(t, true, public["is_model_mapped"])
	adminInfo, _ := snapshot["admin_info"].(map[string]any)
	assert.Equal(t, "claude-sonnet-4-6", adminInfo["upstream_model_name"], "admins keep the value they had")
}

// Non-2xx upstream responses are observed before they become errors, so the
// error log carries ursp.err and an evidence row; the error itself is exactly
// what RelayErrorHandler returns for the same response (spec C3).
func TestRelayErrorHandlerWithAuditObservesErrorBodies(t *testing.T) {
	isolatedAuditKey(t, 987665, "gpt")
	body := `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limited"}}`
	newResp := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": {"3"}, "Authorization": {"Bearer secret"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}
	info := &relaycommon.RelayInfo{StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5", ChannelId: 987665}}
	got := RelayErrorHandlerWithAudit(context.Background(), info, newResp(), false)
	want := RelayErrorHandler(context.Background(), newResp(), false)
	require.NotNil(t, got)
	assert.Equal(t, want.Error(), got.Error())
	assert.Equal(t, want.StatusCode, got.StatusCode)

	assert.Equal(t, body, info.UpstreamResponseExcerpt)
	assert.Contains(t, info.UpstreamResponseHeaders, `"Retry-After":"3"`)
	assert.NotContains(t, info.UpstreamResponseHeaders, "secret")

	var queued *model.UpstreamEvidence
	original := enqueueUpstreamEvidence
	enqueueUpstreamEvidence = func(e *model.UpstreamEvidence) bool { queued = e; return true }
	t.Cleanup(func() { enqueueUpstreamEvidence = original })
	other := model.NewLogOther()
	AppendUpstreamResponseAuditForError(nil, info, other, string(got.GetErrorType()))
	ursp := urspOf(t, other)
	assert.Equal(t, "error_response", ursp["h"])
	assert.Equal(t, "partial", ursp["obs"])
	assert.NotEmpty(t, ursp["err"])
	require.NotNil(t, queued)
	assert.Contains(t, queued.Note, "error: ")
	assert.Equal(t, body, queued.ResponseExcerpt)
}

// Owner decision 2026-09-28: a pinned build answered by the bare alias (the
// official DeepSeek behaviour) is recorded but is not a mismatch.
func TestAuditPinnedToAliasIsRecordedNotMismatch(t *testing.T) {
	isolatedAuditKey(t, 987667, "deepseek")
	ursp, ev := runAuditExchange(t, auditExchange{channelId: 987667, requestId: "pin-1",
		sent: "deepseek-v4-pro-0813", models: []string{"deepseek-v4-pro"}, id: "chatcmpl-pinned0001"})
	assert.Equal(t, VerdictPinnedToAlias, ursp["kind"])
	assert.Equal(t, false, ursp["mismatch"])
	assert.NotContains(t, ursp, "tier_downgrade")
	assert.Equal(t, "normal", ev.Kind)
	assert.Equal(t, "pinned_to_alias", ev.Note)
}

// A channel that stamps every answer with the same old created (#109) makes
// stale_created meaningless: once the same stamp shows up with different ids
// and outputs, it no longer corroborates a duplicate output (spec review L-1).
func TestAuditConstantCreatedDoesNotCorroborate(t *testing.T) {
	const ch = 987668
	isolatedAuditKey(t, ch, "gpt")
	constantOld := time.Now().Unix() - 30*86400
	long := strings.Repeat("A long deterministic answer that repeats word for word. ", 5)
	run := func(req, id, output string) (map[string]any, *model.UpstreamEvidence) {
		return runAuditExchange(t, auditExchange{channelId: ch, requestId: req, models: []string{"gpt-5"},
			id: id, created: constantOld, output: output, promptHash: strings.Repeat("c", 64)})
	}
	run("req-1", "chatcmpl-constcr01", strings.Repeat("first distinct answer ", 12))
	run("req-2", "chatcmpl-constcr02", long)
	ursp, ev := run("req-3", "chatcmpl-constcr03", long)
	assert.Equal(t, true, ursp["stale_created"], "still recorded as a flag")
	assert.Equal(t, true, ursp["dup_output"])
	assert.NotContains(t, ursp, "replay")
	assert.Equal(t, "normal", ev.Kind)
}

// Cold start on a #109-shaped channel (constant old created, fresh distinct
// ids): before the constant-created tracker has seen two different answers,
// the first repeated long answer must not become a replay (review round 5).
func TestAuditConstantCreatedColdStartNoReplay(t *testing.T) {
	const ch = 987670
	isolatedAuditKey(t, ch, "gpt")
	constantOld := time.Now().Unix() - 30*86400
	long := strings.Repeat("A long deterministic answer that repeats word for word. ", 5)
	for _, prompt := range []string{strings.Repeat("d", 64), strings.Repeat("e", 64)} {
		ursp, ev := runAuditExchange(t, auditExchange{channelId: ch, requestId: "cold-2-" + prompt[:1], models: []string{"gpt-5"},
			id: "chatcmpl-x1090000" + prompt[:1], created: constantOld, output: long, promptHash: prompt})
		if prompt[0] == 'd' {
			continue // first answer: nothing to compare with
		}
		assert.Equal(t, true, ursp["stale_created"])
		assert.Equal(t, true, ursp["dup_output"])
		assert.NotContains(t, ursp, "replay")
		assert.Equal(t, "normal", ev.Kind)
	}

	// a real cross-id cache replay still counts: the original answer was fresh
	const ch2 = 987671
	isolatedAuditKey(t, ch2, "gpt")
	// (the original was fresh 22 days ago; the copy hands back its stamp)
	then := time.Now().Add(-22 * 24 * time.Hour)
	runAuditExchange(t, auditExchange{channelId: ch2, requestId: "cache-1", models: []string{"gpt-5"},
		id: "chatcmpl-orig00001", created: then.Unix(), output: long, promptHash: strings.Repeat("f", 64), startedAt: then})
	ursp, ev := runAuditExchange(t, auditExchange{channelId: ch2, requestId: "cache-2", models: []string{"gpt-5"},
		id: "chatcmpl-copy00001", created: then.Unix(), output: long, promptHash: strings.Repeat("g", 64)})
	assert.Equal(t, true, ursp["replay"])
	assert.Equal(t, "replay_suspect", ev.Kind)
}

// Rule (b): the same stale created remembered for the same id corroborates a
// same-id replay even on a channel already known to stamp a constant created.
func TestAuditSameStaleCreatedOnConstantCreatedChannel(t *testing.T) {
	const ch = 987672
	isolatedAuditKey(t, ch, "gpt")
	old := time.Now().Unix() - 22*86400
	prompt := func(c string) string { return strings.Repeat(c, 64) }
	run := func(req, id, output, p string) (map[string]any, *model.UpstreamEvidence) {
		return runAuditExchange(t, auditExchange{channelId: ch, requestId: req, models: []string{"gpt-5"},
			id: id, created: old, output: output, promptHash: p})
	}
	// mark the channel constant-created: same stamp, two ids, two outputs
	run("m-1", "chatcmpl-mark00001", "first distinct answer", prompt("h"))
	run("m-2", "chatcmpl-mark00002", "second distinct answer", prompt("i"))
	// the #148 shape on that channel
	greeting := "Hello! How can I help you with it today?"
	run("g-1", "chatcmpl-greet0148", greeting, prompt("j"))
	ursp, ev := run("g-2", "chatcmpl-greet0148", greeting, prompt("k"))
	assert.Equal(t, true, ursp["replay"])
	assert.Equal(t, "replay_suspect", ev.Kind)
}

// provider_switch notes keep exactly the flip text the watchdog groups on;
// other kinds put the signals first so the 255-char cut never drops them
// (spec review L-3, L-4).
func TestAuditNoteFormats(t *testing.T) {
	const ch = 987669
	isolatedAuditKey(t, ch, "gpt")
	now := time.Now().Unix()
	azure := map[string][]string{"Apim-Request-Id": {"x"}}
	relay := map[string][]string{"X-New-Api-Version": {"v1"}}
	runAuditExchange(t, auditExchange{channelId: ch, requestId: "n-1", models: []string{"gpt-5"}, id: "chatcmpl-note00001", headers: azure, created: now})
	_, ev := runAuditExchange(t, auditExchange{channelId: ch, requestId: "n-2", models: []string{"gpt-5"}, id: "chatcmpl-note00001",
		headers: relay, created: now - 30*86400, output: "fresh"})
	assert.Equal(t, "provider_switch", ev.Kind)
	// one row: the source flip; losing the Azure headers once is not a
	// backend switch (hysteresis), so the note is the source flip alone
	assert.Equal(t, "azure -> newapi_relay", ev.Note)

	models := make([]string, 0, 8)
	for i := range 8 {
		models = append(models, "gpt-5-mini-variant-with-a-long-name-"+strconv.Itoa(i))
	}
	_, ev = runAuditExchange(t, auditExchange{channelId: ch, requestId: "n-3", stream: true, models: models,
		id: "chatcmpl-note00001", created: now - 30*86400})
	assert.True(t, strings.HasPrefix(ev.Note, "signals: dup_id,stale_created"), ev.Note)
}

// A cache that hands the same originally-fresh answer back several times is a
// replay every time: a replayed copy keeps the fresh answer's stamp and is
// remembered as fresh (review round 6, LOW 1).
func TestAuditRepeatedCacheReplaysAllCount(t *testing.T) {
	const ch = 987674
	isolatedAuditKey(t, ch, "gpt")
	then := time.Now().Add(-22 * 24 * time.Hour)
	answer := strings.Repeat("An answer cached by the relay and served again later. ", 5)
	run := func(req, id string, started time.Time) (map[string]any, *model.UpstreamEvidence) {
		return runAuditExchange(t, auditExchange{channelId: ch, requestId: req, models: []string{"gpt-5"}, id: id,
			created: then.Unix(), output: answer, promptHash: strings.Repeat(req[len(req)-1:], 64), startedAt: started})
	}
	ursp, _ := run("req-a", "chatcmpl-cachedA01", then) // fresh when first produced
	assert.NotContains(t, ursp, "stale_created")
	for _, req := range []string{"req-b", "req-c", "req-d"} {
		ursp, ev := run(req, "chatcmpl-"+req+"-x01", time.Time{})
		assert.Equal(t, true, ursp["stale_created"], req)
		assert.Equal(t, true, ursp["replay"], req)
		assert.Equal(t, "replay_suspect", ev.Kind, req)
	}
}

// A #109-shaped channel stamps every answer with one old constant and later
// switches to another constant: an identical long answer across the switch is
// not a replay, because the earlier copy was already stale (review round 6,
// LOW 2).
func TestAuditConstantStampSwitchIsNotReplay(t *testing.T) {
	const ch = 987675
	isolatedAuditKey(t, ch, "gpt")
	stampX := time.Now().Unix() - 40*86400
	stampY := stampX + 86400
	long := strings.Repeat("A deterministic long answer that repeats word for word. ", 5)
	run := func(req, id, output string, created int64) (map[string]any, *model.UpstreamEvidence) {
		return runAuditExchange(t, auditExchange{channelId: ch, requestId: req, models: []string{"gpt-5"}, id: id,
			created: created, output: output, promptHash: strings.Repeat("p", 64)})
	}
	run("req-1", "chatcmpl-stampX001", long, stampX)
	run("req-2", "chatcmpl-stampX002", strings.Repeat("another answer ", 20), stampX)
	ursp, ev := run("req-3", "chatcmpl-stampY003", long, stampY)
	assert.Equal(t, true, ursp["dup_output"])
	assert.Equal(t, true, ursp["stale_created"])
	assert.NotContains(t, ursp, "replay")
	assert.Equal(t, "normal", ev.Kind)
}

// One backend of a multi-backend channel with a clock skewed by more than
// 120 s answers a prompt identically after a fresh answer from a healthy
// backend: its own stamp differs from the earlier answer's, so it is not a
// replay — a real cache hands back the original stamp (review round 7).
func TestAuditSkewedBackendIdenticalAnswerIsNotReplay(t *testing.T) {
	const ch = 987676
	isolatedAuditKey(t, ch, "gpt")
	now := time.Now().Unix()
	answer := strings.Repeat("A deterministic long answer both backends produce alike. ", 5)
	run := func(req, id string, created int64) (map[string]any, *model.UpstreamEvidence) {
		return runAuditExchange(t, auditExchange{channelId: ch, requestId: req, models: []string{"gpt-5"}, id: id,
			created: created, output: answer, promptHash: strings.Repeat("s", 64)})
	}
	run("req-healthy", "chatcmpl-healthy01", now)
	ursp, ev := run("req-skewed", "chatcmpl-skewed001", now-300)
	assert.Equal(t, true, ursp["dup_output"])
	assert.Equal(t, true, ursp["stale_created"])
	assert.NotContains(t, ursp, "replay")
	assert.Equal(t, "normal", ev.Kind)
}

// ursp.extra_keys is bounded in count and key length: an upstream controls both.
func TestAuditExtraKeysAreBounded(t *testing.T) {
	isolatedAuditKey(t, 987677, "gpt")
	var b strings.Builder
	b.WriteString(`{"id":"chatcmpl-extra0001","object":"chat.completion","model":"gpt-5","choices":[]`)
	// 30 long keys sharing one 200-char prefix collapse to a single cut name;
	// 30 distinct short keys are capped at 20
	for i := range 30 {
		b.WriteString(`,"` + strings.Repeat("k", 200) + strconv.Itoa(i) + `":1`)
		b.WriteString(`,"x` + strconv.Itoa(i) + `":1`)
	}
	b.WriteString(`}`)
	ursp, _ := runAuditExchange(t, auditExchange{channelId: 987677, requestId: "x-1", models: []string{"gpt-5"},
		id: "chatcmpl-extra0001", excerpt: b.String()})
	keys, ok := ursp["extra_keys"].([]string)
	require.True(t, ok)
	assert.Len(t, keys, 20)
	seen := map[string]bool{}
	for _, k := range keys {
		assert.LessOrEqual(t, len([]rune(k)), 64)
		assert.False(t, seen[k], "duplicate extra key %q", k)
		seen[k] = true
	}
	assert.True(t, seen[strings.Repeat("k", 64)], "the collapsed long key is kept once")
}

// auditFrames feeds real-shaped upstream frames through the observation layer
// (every frame, as the stream handlers do; the last one is the excerpt) and
// returns the ursp block and queued evidence of the resulting audit. The
// sent model is the last model the frames declare (an honest echo), so these
// cases isolate the backend decision from the model verdict.
func auditFrames(t *testing.T, channelId, channelType int, headers map[string][]string, frames []string, errorType string) (map[string]any, *model.UpstreamEvidence) {
	t.Helper()
	var queued *model.UpstreamEvidence
	original := enqueueUpstreamEvidence
	enqueueUpstreamEvidence = func(e *model.UpstreamEvidence) bool { queued = e; return true }
	t.Cleanup(func() { enqueueUpstreamEvidence = original })
	sent := "gpt-5"
	for _, f := range frames {
		if m := gjson.Get(f, "model").String(); m != "" {
			sent = m
		}
		if m := gjson.Get(f, "response.model").String(); m != "" {
			sent = m
		}
		if m := gjson.Get(f, "message.model").String(); m != "" {
			sent = m
		}
	}
	info := &relaycommon.RelayInfo{StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{
		UpstreamModelName: sent, ChannelId: channelId, ChannelType: channelType,
	}}
	info.SnapshotUpstreamWire(sent, nil)
	for _, f := range frames {
		model := gjson.Get(f, "model").String()
		id := gjson.Get(f, "id").String()
		if r := gjson.Get(f, "response"); r.IsObject() {
			model, id = r.Get("model").String(), r.Get("id").String()
		}
		if m := gjson.Get(f, "message"); m.IsObject() {
			model, id = m.Get("model").String(), m.Get("id").String()
		}
		info.ObserveUpstreamResponseFrame(f, model, id, 0, "")
	}
	info.SetUpstreamResponseForensics(frames[len(frames)-1], headers)
	other := model.NewLogOther()
	appendUpstreamResponseAudit(nil, info, other, errorType)
	return urspOf(t, other), queued
}

var (
	relayOnlyHeaders = map[string][]string{"X-Oneapi-Request-Id": {"20260928000000000000000"}}

	// Azure Responses API terminal event behind a NewAPI hop that strips the
	// Azure headers (channel #66 since 2026-09-23)
	azureResponsesCompleted = `{"type":"response.completed","response":{"id":"resp_0a1b2c3d4e5f","object":"response","created_at":1790000000,"status":"completed","model":"gpt-5.1","output":[{"type":"message","content":[{"type":"output_text","text":"Hello!"}]}],"content_filters":[{"blocked":false,"source_type":"completion","content_filter_results":{"hate":{"filtered":false,"severity":"safe"},"sexual":{"filtered":false,"severity":"safe"},"violence":{"filtered":false,"severity":"safe"},"self_harm":{"filtered":false,"severity":"safe"},"protected_material_text":{"filtered":false,"detected":false}},"content_filter_offsets":{"start_offset":0,"end_offset":6,"check_offset":0}}],"usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}}`

	// Azure chat stream: prompt_filter_results in the first chunk (empty
	// choices, empty model), content_filter_results in content chunks, and
	// a final usage chunk without any marker
	azureChatStream = []string{
		`{"choices":[],"created":0,"id":"","model":"","object":"","prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"},"jailbreak":{"filtered":false,"detected":false}}}]}`,
		`{"choices":[{"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}},"delta":{"content":"Hi"},"finish_reason":null,"index":0}],"created":1790000000,"id":"chatcmpl-AzUre0001","model":"gpt-4o-2024-11-20","object":"chat.completion.chunk","system_fingerprint":"fp_ee1d74bde0"}`,
		`{"choices":[{"content_filter_results":{},"delta":{},"finish_reason":"stop","index":0}],"created":1790000000,"id":"chatcmpl-AzUre0001","model":"gpt-4o-2024-11-20","object":"chat.completion.chunk"}`,
		`{"choices":[],"created":1790000000,"id":"chatcmpl-AzUre0001","model":"gpt-4o-2024-11-20","object":"chat.completion.chunk","usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`,
	}
)

// Channel #66: Azure behind a NewAPI hop without Azure headers is recognised
// from the body; the hop stays newapi_relay.
func TestAuditAzureResponsesBodyBehindRelay(t *testing.T) {
	isolatedAuditKey(t, 987678, "gpt")
	ursp, ev := auditFrames(t, 987678, constant.ChannelTypeOpenAI, relayOnlyHeaders, []string{azureResponsesCompleted}, "")
	assert.Equal(t, "newapi_relay", ursp["src"])
	assert.Equal(t, "azure", ursp["backend"])
	assert.Equal(t, BackendByBody, ursp["backend_by"])
	assert.Equal(t, BackendConfMedium, ursp["backend_conf"])
	assert.NotContains(t, ursp, "backend_conflict")
	require.NotNil(t, ev)
	assert.Equal(t, "newapi_relay", ev.Source)
}

// Every frame is scanned: the Azure chat markers sit in the first and content
// chunks, while the final usage chunk (the excerpt) carries none.
func TestAuditAzureChatStreamMarkersInEarlierFrames(t *testing.T) {
	isolatedAuditKey(t, 987679, "gpt")
	ursp, _ := auditFrames(t, 987679, constant.ChannelTypeOpenAI, relayOnlyHeaders, azureChatStream, "")
	assert.Equal(t, "azure", ursp["backend"])
	assert.Equal(t, BackendByBody, ursp["backend_by"])
	assert.Equal(t, "gpt-4o-2024-11-20", ursp["model"], "the empty-model first chunk does not count as a declaration")
}

func TestAuditPlainRelayHasNoBackendEvidence(t *testing.T) {
	isolatedAuditKey(t, 987680, "gpt")
	ursp, _ := auditFrames(t, 987680, constant.ChannelTypeOpenAI, relayOnlyHeaders,
		[]string{`{"id":"chatcmpl-plain0001","object":"chat.completion","model":"gpt-5","choices":[{"message":{"content":"Hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`}, "")
	assert.Equal(t, "gpt_relay", ursp["backend"])
	assert.Equal(t, BackendByFallback, ursp["backend_by"])
	assert.Equal(t, BackendConfNone, ursp["backend_conf"])
}

// Bedrock without msg_bdrk_ ids: amazon-bedrock-invocationMetrics on the
// final event identifies it.
func TestAuditBedrockMetricsIdentifyAWS(t *testing.T) {
	isolatedAuditKey(t, 987681, "claude")
	ursp, _ := auditFrames(t, 987681, constant.ChannelTypeAnthropic, nil, []string{
		`{"type":"message_start","message":{"id":"msg_01XyZabc","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":10,"outputTokenCount":2,"invocationLatency":512,"firstByteLatency":300}}`,
	}, "")
	assert.Equal(t, "aws", ursp["backend"])
	assert.Equal(t, BackendByBody, ursp["backend_by"])
}

func TestAuditDeepSeekUsageFieldsIdentifyDeepSeek(t *testing.T) {
	isolatedAuditKey(t, 987682, "deepseek")
	ursp, _ := auditFrames(t, 987682, constant.ChannelTypeOpenAI, relayOnlyHeaders,
		[]string{`{"id":"5f1c7c3e-0b4b-4f5e-9d61-3c2a1f0e9b7d","object":"chat.completion","model":"deepseek-chat","choices":[{"message":{"content":"Hi"}}],"usage":{"prompt_tokens":12,"completion_tokens":1,"total_tokens":13,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":12}}`}, "")
	assert.Equal(t, "deepseek", ursp["backend"])
	assert.Equal(t, BackendByBody, ursp["backend_by"])
	assert.NotContains(t, ursp, "backend_conflict", "the deepseek model name agrees")
}

func TestAuditHeaderAndBodyConflictIsRecorded(t *testing.T) {
	isolatedAuditKey(t, 987683, "gpt")
	ursp, _ := auditFrames(t, 987683, constant.ChannelTypeOpenAI, map[string][]string{"Openai-Processing-Ms": {"120"}}, azureChatStream, "")
	assert.Equal(t, "openai", ursp["backend"])
	assert.Equal(t, BackendByHeader, ursp["backend_by"])
	assert.Equal(t, []string{"openai:header", "azure:body"}, ursp["backend_conflict"])
}

func TestAuditAzureChannelTypeIsStrong(t *testing.T) {
	isolatedAuditKey(t, 987684, "gpt")
	ursp, _ := auditFrames(t, 987684, constant.ChannelTypeAzure, nil, azureChatStream, "")
	assert.Equal(t, "azure", ursp["src"])
	assert.Equal(t, "azure", ursp["backend"])
	assert.Equal(t, BackendByChannelType, ursp["backend_by"])
	assert.Equal(t, BackendConfStrong, ursp["backend_conf"])
}

// A NewAPI usage field identifies the hop when no header does, never the backend.
func TestAuditNewAPIUsageFieldIdentifiesHopOnly(t *testing.T) {
	isolatedAuditKey(t, 987685, "claude")
	ursp, _ := auditFrames(t, 987685, constant.ChannelTypeOpenAI, nil,
		[]string{`{"id":"chatcmpl-hop00001","object":"chat.completion","model":"claude-sonnet-4-5","choices":[{"message":{"content":"Hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4,"claude_cache_creation_5_m_tokens":0}}`}, "")
	assert.Equal(t, "newapi_relay", ursp["src"])
	assert.Equal(t, "gpt_relay", ursp["backend"])
	assert.Equal(t, BackendByFallback, ursp["backend_by"])
}

// An error body's fingerprints never decide the backend (and error attempts
// never feed the trackers).
func TestAuditErrorBodyMarkersDoNotSetBackend(t *testing.T) {
	isolatedAuditKey(t, 987686, "gpt")
	ursp, _ := auditFrames(t, 987686, constant.ChannelTypeOpenAI, relayOnlyHeaders,
		[]string{`{"error":{"code":"content_filter","message":"filtered","innererror":{"code":"ResponsibleAIPolicyViolation","content_filter_result":{"hate":{"filtered":true,"severity":"high"}}},"content_filter_results":{"hate":{"filtered":true}}}}`}, "upstream_error")
	assert.NotEqual(t, "azure", ursp["backend"])
	assert.Equal(t, BackendByFallback, ursp["backend_by"])
}

// Markers belong to one attempt: a retry on a plain relay starts clean.
func TestUpstreamBodyMarkersResetPerAttempt(t *testing.T) {
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5"}}
	info.ObserveUpstreamResponseFrame(azureChatStream[0], "", "", 0, "")
	require.True(t, info.UpstreamAudit.BodyMarkers.Has(relaycommon.BodyMarkerAzureContentFilter))
	info.ResetUpstreamObservation()
	assert.Zero(t, info.UpstreamAudit.BodyMarkers)
}

// The tracker follows the decided backend with hysteresis (review M3): a
// healthy Azure channel whose answers sometimes lack the annotations records
// nothing; 20 consecutive answers without evidence record ONE "evidence lost"
// row, and the evidence coming back one "restored" row.
func TestAuditBackendTrackerFollowsDecidedBackend(t *testing.T) {
	const ch = 987687
	isolatedAuditKey(t, ch, "gpt")
	plain := []string{`{"id":"chatcmpl-plain0002","object":"chat.completion","model":"gpt-4o-2024-11-20","choices":[{"message":{"content":"Hi"}}]}`}
	var rows []*model.UpstreamEvidence
	run := func(frames []string) map[string]any {
		ursp, ev := auditFrames(t, ch, constant.ChannelTypeOpenAI, relayOnlyHeaders, frames, "")
		if ev != nil && ev.Kind == "provider_switch" {
			rows = append(rows, ev)
		}
		return ursp
	}
	for range 4 {
		run(azureChatStream)
		run(plain)
	}
	assert.Empty(t, rows, "alternating healthy / missing annotations is not a switch")
	run(azureChatStream) // a healthy answer resets the count

	for i := range backendEvidenceLostAfter {
		ursp := run(plain)
		if i < backendEvidenceLostAfter-1 {
			assert.NotContains(t, ursp, "backend_flip")
		} else {
			assert.Equal(t, "azure->(none)", ursp["backend_flip"])
		}
	}
	require.Len(t, rows, 1)
	assert.Equal(t, "backend evidence lost: azure -> (none) after 20", rows[0].Note)
	run(plain)
	assert.Len(t, rows, 1, "the lost state does not repeat")

	ursp := run(azureChatStream)
	assert.Equal(t, "(none)->azure", ursp["backend_flip"])
	require.Len(t, rows, 2)
	assert.Equal(t, "backend evidence restored: (none) -> azure", rows[1].Note)
}

// A change between two POSITIVE backends is recorded immediately.
func TestTrackBackendFlipPositiveChangeIsImmediate(t *testing.T) {
	key := isolatedAuditKey(t, 987688, "gpt")
	assert.Empty(t, trackBackendFlip(key, "azure", BackendByBody, nil).flip)
	assert.Empty(t, trackBackendFlip(key, "gpt_relay", BackendByFallback, nil).flip, "one missing annotation is not a switch")
	ev := trackBackendFlip(key, "aws", BackendByBody, nil)
	assert.Equal(t, "azure->aws", ev.flip)
	assert.Equal(t, "backend: azure -> aws", ev.note)
	assert.Equal(t, "aws->deepseek", trackBackendFlip(key, "deepseek", BackendByModelName, nil).flip)
}

// A vendor prefix never turns an answer without vendor evidence into
// evidence: an Azure relay answering openai/gpt-4o alternates
// azure/openai (body) and gpt_relay/openai (fallback) — no rows; 20 such
// fallbacks record one "lost" row, and only real evidence restores it.
func TestTrackBackendFlipPrefixDoesNotBypassHysteresis(t *testing.T) {
	key := isolatedAuditKey(t, 987689, "gpt")
	var events []backendTrackEvent
	track := func(backend, by string) {
		if ev := trackBackendFlip(key, backend, by, nil); ev.flip != "" {
			events = append(events, ev)
		}
	}
	for range 10 {
		track("azure/openai", BackendByBody)
		track("gpt_relay/openai", BackendByFallback)
	}
	assert.Empty(t, events, "prefixed alternation body <-> fallback is not a switch")

	track("azure/openai", BackendByBody)
	for range backendEvidenceLostAfter + 5 {
		track("gpt_relay/openai", BackendByFallback)
	}
	require.Len(t, events, 1)
	assert.Equal(t, "backend evidence lost: azure/openai -> (none) after 20", events[0].note)

	track("gpt_relay/openai", BackendByFallback)
	assert.Len(t, events, 1, "a prefixed fallback does not restore")
	track("azure/openai", BackendByBody)
	require.Len(t, events, 2)
	assert.Equal(t, "backend evidence restored: (none) -> azure/openai", events[1].note)
}

// A key that starts without vendor evidence is seeded all the same, so its
// first real switch is recorded; fallbacks among themselves stay quiet.
func TestTrackBackendFlipSeedsFromFallback(t *testing.T) {
	key := isolatedAuditKey(t, 987690, "gpt")
	for range 5 {
		assert.Empty(t, trackBackendFlip(key, "gpt_relay", BackendByFallback, nil).flip)
	}
	assert.Equal(t, "gpt_relay->openrouter", trackBackendFlip(key, "openrouter", BackendByIdShape, nil).flip)

	claude := isolatedAuditKey(t, 987691, "claude")
	assert.Empty(t, trackBackendFlip(claude, "claude_relay", BackendByFallback, nil).flip)
	assert.Equal(t, "claude_relay->aws", trackBackendFlip(claude, "aws", BackendByIdShape, nil).flip, "msg_ -> msg_bdrk_")

	prefixed := isolatedAuditKey(t, 987692, "gpt")
	assert.Empty(t, trackBackendFlip(prefixed, "gpt_relay/vendorx", BackendByFallback, nil).flip)
	assert.Empty(t, trackBackendFlip(prefixed, "gpt_relay", BackendByFallback, nil).flip, "prefix absent is the same backend")
	assert.Empty(t, trackBackendFlip(prefixed, "gpt_relay/vendorx", BackendByFallback, nil).flip, "prefix present again")
}

// Azure-hosted DeepSeek-R1 behind a relay with partial Azure annotations:
// annotated replies decide azure (body) with deepseek (model name) next to
// it, unannotated ones only deepseek (model name). The body proves the
// platform and the name the model; they do not contradict, so alternation
// records nothing; 20 unannotated replies record one "lost" row; a reply
// from another vendor (gen- id) still flips immediately.
func TestAuditAzureHostedDeepSeekAlternationIsNotASwitch(t *testing.T) {
	const ch = 987693
	isolatedAuditKey(t, ch, "deepseek")
	annotated := []string{`{"id":"chatcmpl-r1az0001","object":"chat.completion","model":"DeepSeek-R1","prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}],"choices":[{"index":0,"message":{"content":"Hi"},"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}]}`}
	unannotated := []string{`{"id":"chatcmpl-r1az0002","object":"chat.completion","model":"DeepSeek-R1","choices":[{"index":0,"message":{"content":"Hi"}}]}`}
	var rows []*model.UpstreamEvidence
	run := func(frames []string) map[string]any {
		ursp, ev := auditFrames(t, ch, constant.ChannelTypeOpenAI, relayOnlyHeaders, frames, "")
		if ev != nil && ev.Kind == "provider_switch" {
			rows = append(rows, ev)
		}
		return ursp
	}
	ursp := run(annotated)
	assert.Equal(t, "azure", ursp["backend"])
	assert.Equal(t, []string{"azure:body", "deepseek:model_name"}, ursp["backend_conflict"])
	for range 5 {
		run(unannotated)
		run(annotated)
	}
	assert.Empty(t, rows, "platform evidence and model naming alternating is not a switch")

	for range backendEvidenceLostAfter {
		run(unannotated)
	}
	require.Len(t, rows, 1)
	assert.Equal(t, "backend evidence lost: azure -> (none) after 20", rows[0].Note)

	ursp = run([]string{`{"id":"gen-1790000000-AbCdEf","object":"chat.completion","model":"DeepSeek-R1","choices":[{"index":0,"message":{"content":"Hi"}}]}`})
	assert.Equal(t, "azure->openrouter", ursp["backend_flip"])
	require.Len(t, rows, 2)
}

// A same-vendor answer never downgrades the provenance: aws known from the
// body stays body-backed after an aws id-shape answer, so a later fallback
// still goes through hysteresis (review LOW 1).
func TestTrackBackendFlipKeepsStrongestProvenance(t *testing.T) {
	key := isolatedAuditKey(t, 987694, "claude")
	assert.Empty(t, trackBackendFlip(key, "aws", BackendByBody, nil).flip)
	assert.Empty(t, trackBackendFlip(key, "aws", BackendByIdShape, nil).flip)
	assert.Empty(t, trackBackendFlip(key, "claude_relay", BackendByFallback, nil).flip, "one answer without evidence")
}

// After "lost", a same-vendor weak answer stays under the lost state (no
// row); only header/body/channel-type evidence records "restored" (LOW 2).
func TestTrackBackendFlipLostNeedsRealEvidenceToRestore(t *testing.T) {
	key := isolatedAuditKey(t, 987695, "claude")
	trackBackendFlip(key, "aws", BackendByBody, nil)
	var lost backendTrackEvent
	for range backendEvidenceLostAfter {
		lost = trackBackendFlip(key, "claude_relay", BackendByFallback, nil)
	}
	require.Equal(t, "aws->(none)", lost.flip)
	assert.Empty(t, trackBackendFlip(key, "aws", BackendByIdShape, nil).flip, "weak same-vendor evidence does not restore")
	assert.Empty(t, trackBackendFlip(key, "claude_relay", BackendByFallback, nil).flip, "still lost")
	assert.Equal(t, "(none)->aws", trackBackendFlip(key, "aws", BackendByBody, nil).flip)
}

// A backend known only from its model name (deepseek) followed by Azure body
// evidence is recorded as a switch even though that evidence names deepseek
// as a companion: "model name unchanged, backend swapped" must surface.
// After a restart whose first reply was weak-only this is the accepted one
// row per key.
func TestTrackBackendFlipWeakBackendThenEvidenceIsASwitch(t *testing.T) {
	established := isolatedAuditKey(t, 987696, "deepseek")
	for range 500 {
		assert.Empty(t, trackBackendFlip(established, "deepseek", BackendByModelName, nil).flip)
	}
	assert.Equal(t, "deepseek->azure", trackBackendFlip(established, "azure", BackendByBody, []string{"deepseek"}).flip)

	restarted := isolatedAuditKey(t, 987697, "deepseek")
	assert.Empty(t, trackBackendFlip(restarted, "deepseek", BackendByModelName, nil).flip)
	ev := trackBackendFlip(restarted, "azure", BackendByBody, []string{"deepseek"})
	assert.Equal(t, "deepseek->azure", ev.flip)
	assert.Equal(t, "backend: deepseek -> azure", ev.note)
	assert.Empty(t, trackBackendFlip(restarted, "deepseek", BackendByModelName, nil).flip, "afterwards deepseek is a companion: no evidence this time")
}

// Evidence returning for a different vendor after "lost" is a switch, not a
// restoration.
func TestTrackBackendFlipEvidenceForAnotherVendorAfterLost(t *testing.T) {
	key := isolatedAuditKey(t, 987698, "gpt")
	trackBackendFlip(key, "azure", BackendByBody, nil)
	for range backendEvidenceLostAfter {
		trackBackendFlip(key, "gpt_relay", BackendByFallback, nil)
	}
	ev := trackBackendFlip(key, "aws", BackendByBody, nil)
	assert.Equal(t, "azure->aws", ev.flip)
	assert.Equal(t, "backend: azure -> aws (after evidence lost)", ev.note)
}
