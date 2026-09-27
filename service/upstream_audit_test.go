package service

import (
	"testing"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/stretchr/testify/assert"
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
		{"gpt-5-2025-08-07", "gpt-5", false}, // reversed: pin not honoured (pinned_to_alias)
		// DeepSeek date build
		{"deepseek-v4-flash", "deepseek-v4-flash-0731", true},
		// -latest alias (suffix form)
		{"claude-sonnet-4-6", "claude-sonnet-4-6-latest", true},
		// -latest rolling alias resolving to a concrete same-family version (2026-09-03 #57 real traffic)
		{"gemini-pro-latest", "gemini-3.1-pro-preview", false}, // A2: preview tier was never asked for
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
		{"gemini-pro-latest", "gemini-3.1-pro-preview", VerdictMismatch}, // A2: lower-tier "preview" not in the alias
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
		{"gemini-pro-latest", "gemini-3.1-pro-preview", true, "-latest resolved to a preview tier"},
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
		{"latest pro to preview", "gemini-pro-latest", "gemini-3.1-pro-preview", VerdictMismatch},
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
	ch := 987655
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
	ch := 987654 // avoid clashing with other tests
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
	if got := inferBackendShape(14, "msg_bdrk_abc", "global.anthropic.claude-sonnet-5", false); got != "aws" {
		t.Fatalf("backend = %q, want aws", got)
	}
	if got := inferBackendShape(14, "msg_01abc", "claude-sonnet-5", false); got != "claude_relay" {
		t.Fatalf("backend = %q, want claude_relay", got)
	}
}

func TestTrackBackendFlipIsIndependentOfSource(t *testing.T) {
	ch := 987653
	if _, f := trackBackendFlip(ch, "aws"); f {
		t.Fatal("seed must not flip")
	}
	if _, f := trackSourceFlip(ch, "newapi_relay"); f {
		t.Fatal("source seed must not flip")
	}
	from, f := trackBackendFlip(ch, "claude_relay")
	if !f || from != "aws" {
		t.Fatalf("backend aws -> claude_relay must flip, got from=%q flipped=%v", from, f)
	}
	if _, f := trackSourceFlip(ch, "newapi_relay"); f {
		t.Fatal("source unchanged must not flip when only backend moved")
	}
}

// The error-log path calls the exported wrapper on every recorded error,
// including ones where the request never reached an upstream. It must leave
// the log untouched in that case and must tolerate a nil relayInfo (the
// channel-test path passes one).
func TestAppendUpstreamResponseAuditNoopWithoutObservation(t *testing.T) {
	other := model.NewLogOther()
	AppendUpstreamResponseAuditForError(nil, nil, other, "upstream_error")
	AppendUpstreamResponseAuditForError(nil, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.5"}}, other, "upstream_error")
	if _, ok := other.Snapshot()["root_info"]; ok {
		t.Fatal("root_info written although nothing was observed")
	}
}

// An observed error response yields a ursp block that is labelled as failed,
// still carries the mismatch verdict, and does not feed the flip trackers
// (an error answered with the relay's own headers must not look like a
// provider switch).
func TestAppendUpstreamResponseAuditForErrorLabelsAndSkipsFlip(t *testing.T) {
	const ch = 987652 // unused by the other tracker tests (987653-987655)
	t.Cleanup(func() { srcTracker.Delete(ch); backendTracker.Delete(ch) })
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

// The displayed backend must not contradict an officially-attested source when
// the body shape is neutral; positive shape evidence still wins; relay
// sources never get promoted.
func TestReconcileBackendWithSource(t *testing.T) {
	cases := []struct{ shape, src, want string }{
		{"gpt_relay", "azure", "azure"},            // chatcmpl- body + Azure headers
		{"gpt_relay", "openai", "openai"},          // chatcmpl- body + OpenAI org headers
		{"claude_relay", "anthropic", "anthropic"}, // msg_ body + anthropic-ratelimit headers
		{"unknown", "google", "google"},
		{"gpt_relay", "newapi_relay", "gpt_relay"}, // relay headers say nothing about the backend
		{"gpt_relay", "unknown", "gpt_relay"},
		{"aws", "azure", "aws"},            // msg_bdrk_ behind Azure-looking headers: shape wins
		{"deepseek", "openai", "deepseek"}, // positive model-family evidence wins
		{"openrouter", "newapi_relay", "openrouter"},
	}
	for _, tc := range cases {
		if got := reconcileBackendWithSource(tc.shape, tc.src); got != tc.want {
			t.Errorf("reconcile(%q, %q) = %q, want %q", tc.shape, tc.src, got, tc.want)
		}
	}
}

// End to end through the audit: an Azure-attested response with a chatcmpl id
// displays backend=azure, while the backend flip tracker still sees the raw
// shape (gpt_relay) and does not flip.
func TestAuditBackendFollowsOfficialSourceButTrackerSeesShape(t *testing.T) {
	const ch = 987651
	t.Cleanup(func() { srcTracker.Delete(ch); backendTracker.Delete(ch) })
	azureHeaders := map[string][]string{"Apim-Request-Id": {"x"}, "X-Ms-Region": {"East US 2"}}
	run := func(headers map[string][]string) map[string]interface{} {
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5", ChannelId: ch, ChannelType: 1}}
		info.ObserveUpstreamResponse("gpt-5-2025-08-07", "chatcmpl-abc", 1, "")
		info.SetUpstreamResponseForensics(`{"id":"chatcmpl-abc"}`, headers)
		other := model.NewLogOther()
		appendUpstreamResponseAudit(nil, info, other, "")
		root, _ := other.Snapshot()["root_info"].(map[string]interface{})
		ursp, _ := root["ursp"].(map[string]interface{})
		return ursp
	}
	first := run(azureHeaders)
	if first["src"] != "azure" || first["backend"] != "azure" {
		t.Fatalf("expected src=azure backend=azure, got %+v", first)
	}
	// same shape, relay headers only: displayed backend drops back to the shape,
	// the backend tracker saw gpt_relay both times → no backend flip
	second := run(map[string][]string{"X-New-Api-Version": {"v1"}})
	if second["backend"] != "gpt_relay" {
		t.Fatalf("expected backend=gpt_relay behind a relay, got %+v", second)
	}
	if _, flipped := second["backend_flip"]; flipped {
		t.Fatalf("backend flip must follow the raw shape, not the reconciled label: %+v", second)
	}
	if _, flipped := second["src_flip"]; !flipped {
		t.Fatalf("source flip azure->newapi_relay should still be reported: %+v", second)
	}
	// a real shape change (chatcmpl → msg_bdrk_) behind Azure-looking headers
	// is reported shape→shape on both sides, never with the reconciled label
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-sonnet-5", ChannelId: ch, ChannelType: 1}}
	info.ObserveUpstreamResponse("claude-sonnet-5", "msg_bdrk_01abc", 0, "")
	info.SetUpstreamResponseForensics(`{"id":"msg_bdrk_01abc"}`, azureHeaders)
	other := model.NewLogOther()
	appendUpstreamResponseAudit(nil, info, other, "")
	root, _ := other.Snapshot()["root_info"].(map[string]interface{})
	third, _ := root["ursp"].(map[string]interface{})
	if third["backend"] != "aws" {
		t.Fatalf("positive shape evidence must win: %+v", third)
	}
	if third["backend_flip"] != "gpt_relay->aws" {
		t.Fatalf("backend flip must be shape->shape, got %+v", third)
	}
}
