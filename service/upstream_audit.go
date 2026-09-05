package service

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

// ── upstream response model auditing ─────────────────────────────────────────
// Compares the model we sent upstream with the model the upstream response
// declared, infers the real upstream source from response fingerprints, and
// tracks per-channel source flips. Pure observation: never touches the
// forwarding or billing path. Design mirrors sub2api's tri-state mismatch and
// the watchdog identifyGateway header fingerprints.

// canonicalGrokRuntimeModel maps xAI public aliases onto the runtime build id
// the API reports, so alias↔build echoes don't count as mismatches.
func canonicalGrokRuntimeModel(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "grok-4.5", "grok-4.5-latest", "grok-4.5-build":
		return "grok-4.5-build"
	case "grok-4.6", "grok-4.6-latest", "grok-4.6-build":
		return "grok-4.6-build"
	default:
		return ""
	}
}

// snapshotAliasMatch accepts the vendor alias↔snapshot pattern: one name
// extends the other by "-<date>" (gpt-5 vs gpt-5-2025-08-07, x vs x-0731) or
// by "-latest". Deliberately NOT a general prefix match — an arbitrary suffix
// (deepseek-v4 vs deepseek-v4-flash) is a different model and must still flag.
func snapshotAliasMatch(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if len(a) > len(b) {
		a, b = b, a
	}
	if !strings.HasPrefix(b, a+"-") {
		return false
	}
	suffix := b[len(a)+1:]
	if suffix == "latest" {
		return true
	}
	digits := strings.ReplaceAll(suffix, "-", "")
	if len(digits) < 4 || len(digits) > 8 {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// stripVendorPrefix drops an OpenRouter-style "vendor/" prefix
// (deepseek/deepseek-v4-flash → deepseek-v4-flash) so a relay that echoes the
// namespaced id is not flagged. Only the last path segment is kept.
func stripVendorPrefix(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 && i < len(m)-1 {
		return m[i+1:]
	}
	return m
}

// UpstreamModelsMatchForAudit reports whether the upstream-declared model is
// consistent with the model we sent. Case-insensitive; vendor/ prefixes, known
// vendor runtime aliases and alias↔snapshot date suffixes are canonicalized
// before comparing.
func UpstreamModelsMatchForAudit(sentModel, responseModel string) bool {
	sentModel = stripVendorPrefix(strings.TrimSpace(sentModel))
	responseModel = stripVendorPrefix(strings.TrimSpace(responseModel))
	if sentModel == "" || responseModel == "" {
		return true // nothing to compare — never flag on missing data
	}
	if strings.EqualFold(sentModel, responseModel) {
		return true
	}
	if g := canonicalGrokRuntimeModel(sentModel); g != "" && g == canonicalGrokRuntimeModel(responseModel) {
		return true
	}
	if snapshotAliasMatch(sentModel, responseModel) {
		return true
	}
	if latestFamilyMatch(sentModel, responseModel) {
		return true
	}
	return false
}

// latestFamilyMatch accepts a vendor "-latest" rolling alias resolving to a
// concrete version of the SAME family (gemini-pro-latest → gemini-3.1-pro-preview):
// every non-version token of the alias (minus "latest") must appear in the
// response id. gemini-flash-latest → gemini-3.1-pro-preview still flags
// ("flash" missing), so tier swaps are not masked.
func latestFamilyMatch(sentModel, responseModel string) bool {
	sent := strings.ToLower(sentModel)
	if !strings.HasSuffix(sent, "-latest") {
		return false
	}
	respTokens := map[string]bool{}
	for _, t := range strings.Split(strings.ToLower(responseModel), "-") {
		respTokens[t] = true
	}
	for _, t := range strings.Split(strings.TrimSuffix(sent, "-latest"), "-") {
		if t == "" {
			continue
		}
		if !respTokens[t] {
			return false
		}
	}
	return true
}

// ── upstream source inference ────────────────────────────────────────────────
// Labels are short stable tokens; the frontend maps them to display names.
// Evidence priority: channel protocol type (cryptographically binding, e.g.
// AWS SigV4) > official vendor headers > relay software headers > response id
// shape + model family (weak). Anything else → "unknown".

var (
	uuidIdRe    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	numericIdRe = regexp.MustCompile(`^\d{10,}$`)
)

func headerKeysLower(headersJSON string) map[string]string {
	if headersJSON == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(headersJSON), &m); err != nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

func hasHeaderPrefix(h map[string]string, prefixes ...string) bool {
	for k := range h {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				return true
			}
		}
	}
	return false
}

// InferUpstreamSource returns a source label for the upstream that actually
// produced this response. Weak-evidence inference: relays CAN forge all of
// this, so treat it as a screening signal, not proof. ccmaxHint marks a
// channel with prompt-injection evidence (see injectionDetected) and only
// upgrades an Anthropic-shaped response from claude_relay to ccmax.
func InferUpstreamSource(channelType int, headersJSON, respId, respModel string, ccmaxHint bool) string {
	h := headerKeysLower(headersJSON)
	id := strings.TrimSpace(respId)
	modelLower := strings.ToLower(respModel)

	// 1. channel protocol type — the wire protocol itself pins the backend family
	switch channelType {
	case constant.ChannelTypeAws:
		return "aws"
	case constant.ChannelTypeVertexAi:
		return "vertex"
	case constant.ChannelTypeAzure:
		return "azure"
	}

	// 2. official vendor headers (strongest response-side evidence)
	if h != nil {
		if _, ok := h["x-amzn-requestid"]; ok {
			return "aws"
		}
		if hasHeaderPrefix(h, "x-amz-bedrock") {
			return "aws"
		}
		if _, ok := h["apim-request-id"]; ok {
			return "azure"
		}
		if hasHeaderPrefix(h, "x-ms-region", "x-ms-deployment") {
			return "azure"
		}
		// Claude Max/Pro subscription relays pass through unified rate-limit headers
		if hasHeaderPrefix(h, "anthropic-ratelimit-unified") {
			return "ccmax"
		}
		if _, ok := h["anthropic-organization-id"]; ok {
			return "anthropic"
		}
		if hasHeaderPrefix(h, "anthropic-ratelimit-") {
			return "anthropic"
		}
		if _, ok := h["openai-organization"]; ok {
			return "openai"
		}
		if _, ok := h["openai-processing-ms"]; ok {
			return "openai"
		}
		if _, ok := h["openai-version"]; ok {
			return "openai"
		}
		if hasHeaderPrefix(h, "msh-") { // Moonshot (Kimi) request headers
			return "kimi"
		}
		if hasHeaderPrefix(h, "x-goog-", "x-google-") {
			if channelType == constant.ChannelTypeGemini {
				return "aistudio"
			}
			return "google"
		}
		// relay software fingerprints
		if _, ok := h["x-new-api-version"]; ok {
			return "newapi_relay"
		}
		if _, ok := h["x-oneapi-request-id"]; ok {
			return "newapi_relay"
		}
		if hasHeaderPrefix(h, "x-or-", "x-openrouter") {
			return "openrouter"
		}
		if hasHeaderPrefix(h, "cf-aig-") {
			return "cf_gateway"
		}
	}

	// 3. response id shape + model family (weak)
	switch {
	case strings.HasPrefix(id, "msg_bdrk_"):
		return "aws"
	case strings.HasPrefix(id, "msg_"):
		if ccmaxHint {
			return "ccmax" // anthropic wire shape + injected-prompt evidence = Max subscription relay
		}
		return "claude_relay" // anthropic wire shape without official headers
	case strings.HasPrefix(id, "gen-"):
		return "openrouter"
	case strings.HasPrefix(id, "chatcmpl-"), strings.HasPrefix(id, "resp_"):
		// OpenAI chat / Responses API wire shape without official headers
		switch {
		case strings.Contains(modelLower, "deepseek"):
			return "deepseek"
		case strings.Contains(modelLower, "glm"):
			return "glm"
		case strings.Contains(modelLower, "kimi") || strings.Contains(modelLower, "moonshot"):
			return "kimi"
		default:
			return "gpt_relay"
		}
	case uuidIdRe.MatchString(id):
		if strings.Contains(modelLower, "deepseek") {
			return "deepseek"
		}
		return "unknown"
	case numericIdRe.MatchString(id):
		return "glm" // Zhipu-style timestamp ids
	case channelType == constant.ChannelTypeGemini && id != "":
		return "aistudio"
	}
	return "unknown"
}

// ── per-channel source flip tracking ─────────────────────────────────────────
// A channel whose inferred source flips (az → gpt_relay → az ...) is a strong
// adulteration / backend-swap signal. EVERY known→known flip is recorded as
// evidence (kind=provider_switch) — no rate limiting, per user decision
// 2026-09-02: evidence completeness beats table size. Push-side dedup in the
// watchdog keeps notifications sane. State is in-memory: after a restart the
// first request re-seeds silently (no false alarm).

type channelSrcState struct {
	mu   sync.Mutex
	last string
}

var srcTracker sync.Map // channelId(int) -> *channelSrcState

// srcFamily collapses labels that describe the same backend so request-shape
// dependent relabeling never reads as a backend switch: ccmax is only a
// finer-grained claude_relay (same relay, injection just wasn't visible on a
// request that carried its own large cached prompt).
func srcFamily(s string) string {
	if s == "ccmax" {
		return "claude_relay"
	}
	return s
}

func trackSourceFlip(channelId int, src string) (from string, flipped bool) {
	if channelId == 0 || src == "" || src == "unknown" {
		return "", false
	}
	v, _ := srcTracker.LoadOrStore(channelId, &channelSrcState{})
	st := v.(*channelSrcState)
	st.mu.Lock()
	defer st.mu.Unlock()
	prev := st.last
	st.last = src
	if prev == "" || srcFamily(prev) == srcFamily(src) {
		return "", false
	}
	return prev, true
}

// ── ccmax (Claude Max subscription relay) detection ──────────────────────────
// Such relays strip every Anthropic header, but they MUST inject the Claude Code
// OAuth system prompt (~3-4k tokens) to be accepted upstream. The upstream then
// reports far more input (incl. cache) than we estimated for the client's own
// request. A channel that shows this evidence once is remembered in-memory so
// requests whose own big cached prompts hide the injection still get the ccmax
// label (restart re-seeds on the next visible injection — no false alarms).
var ccmaxChannels sync.Map // channelId(int) -> true

const (
	injectionMinAbs   = 2000 // tokens: above the Claude Code preamble noise floor
	injectionMinRatio = 0.5  // and >50% of our estimate, so tokenizer drift on huge prompts never trips it
)

// injectedTokens is how many input tokens the upstream saw beyond our own
// pre-request estimate; 0 when nothing can be inferred.
func injectedTokens(relayInfo *relaycommon.RelayInfo) int {
	est := relayInfo.GetEstimatePromptTokens()
	total := relayInfo.UpstreamInputTokens + relayInfo.UpstreamCacheCreationTokens + relayInfo.UpstreamCacheReadTokens
	if est <= 0 || total <= est {
		return 0
	}
	return total - est
}

func injectionDetected(est, injected int) bool {
	return est > 0 && injected >= injectionMinAbs && float64(injected) >= injectionMinRatio*float64(est)
}

// appendUpstreamResponseAudit adds the ursp block to other.admin_info and
// persists forensic evidence asynchronously. EVERY observed response is
// persisted (kind=normal for clean ones — user decision 2026-09-02, full
// retention for evidence completeness; normal rows are pruned after
// model.NormalEvidenceRetentionDays, anomalies are kept forever).
func appendUpstreamResponseAudit(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, other *model.LogOther) {
	if relayInfo == nil || other == nil {
		return
	}
	respModel := relayInfo.UpstreamResponseModel
	if respModel == "" && relayInfo.UpstreamResponseId == "" && relayInfo.UpstreamResponseExcerpt == "" {
		return // nothing observed at all
	}
	// respModel may be empty: the upstream sent a terminal event without a model.
	// Tri-state still applies to the mismatch verdict (never flags), but the
	// exchange is recorded (ursp.nomodel + evidence) so it stays auditable.
	sentModel := relayInfo.UpstreamSentModel
	if sentModel == "" && relayInfo.ChannelMeta != nil {
		sentModel = relayInfo.UpstreamModelName
	}
	mismatch := !UpstreamModelsMatchForAudit(sentModel, respModel)

	channelId, channelType := 0, 0
	if relayInfo.ChannelMeta != nil {
		channelId = relayInfo.ChannelMeta.ChannelId
		channelType = relayInfo.ChannelMeta.ChannelType
	}
	// ccmax evidence: sticky per-channel memory, refreshed whenever injection is visible
	anthropicShape := strings.HasPrefix(relayInfo.UpstreamResponseId, "msg_")
	inj := injectedTokens(relayInfo)
	ccmaxHint := false
	if channelId != 0 {
		_, ccmaxHint = ccmaxChannels.Load(channelId)
	}
	if anthropicShape && injectionDetected(relayInfo.GetEstimatePromptTokens(), inj) {
		ccmaxHint = true
		if channelId != 0 {
			ccmaxChannels.Store(channelId, true)
		}
	}
	src := InferUpstreamSource(channelType, relayInfo.UpstreamResponseHeaders, relayInfo.UpstreamResponseId, respModel, ccmaxHint)

	ursp := map[string]interface{}{
		"sent":     sentModel,
		"mismatch": mismatch,
		"src":      src,
	}
	if respModel != "" {
		ursp["model"] = respModel
	} else {
		ursp["nomodel"] = true // upstream terminal event carried no model declaration
	}
	if relayInfo.UpstreamResponseId != "" {
		ursp["id"] = relayInfo.UpstreamResponseId
	}
	if relayInfo.UpstreamSystemFingerprint != "" {
		ursp["sf"] = relayInfo.UpstreamSystemFingerprint
	}
	if relayInfo.UpstreamResponseCreated != 0 {
		ursp["created"] = relayInfo.UpstreamResponseCreated
	}
	from, flipped := trackSourceFlip(channelId, src)
	if flipped {
		ursp["src_flip"] = from + "->" + src
	}
	// admin-only projection: LogOther strips admin_info for non-admin readers
	other.SetAdmin("ursp", ursp)

	kind, note := "normal", ""
	switch {
	case flipped && !mismatch:
		kind = "provider_switch"
		note = from + " -> " + src
	case mismatch && flipped:
		kind = "model_mismatch"
		note = "provider also flipped: " + from + " -> " + src
	case mismatch:
		kind = "model_mismatch"
	}

	requestId := ""
	if ctx != nil {
		requestId = ctx.GetString(common.RequestIdKey)
	}

	// evidence snapshot — copy everything needed before leaving the request scope
	ev := &model.UpstreamEvidence{
		CreatedAt:         common.GetTimestamp(),
		Kind:              kind,
		Note:              note,
		Source:            src,
		RequestId:         requestId,
		ChannelId:         channelId,
		UserId:            relayInfo.UserId,
		TokenId:           relayInfo.TokenId,
		RequestModel:      relayInfo.OriginModelName,
		SentModel:         sentModel,
		ResponseModel:     respModel,
		ResponseId:        relayInfo.UpstreamResponseId,
		SystemFingerprint: relayInfo.UpstreamSystemFingerprint,
		ResponseCreated:   relayInfo.UpstreamResponseCreated,
		IsStream:          relayInfo.IsStream,
		UseTimeMs:         int(time.Since(relayInfo.StartTime).Milliseconds()),
		ResponseExcerpt:   relayInfo.UpstreamResponseExcerpt,
		ResponseHeaders:   relayInfo.UpstreamResponseHeaders,
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				common.SysError("upstream evidence persist panic recovered")
			}
		}()
		if err := model.InsertUpstreamEvidence(ev); err != nil {
			common.SysError("upstream evidence persist failed: " + err.Error())
		} else if kind != "normal" { // normal rows are high-volume — no per-row logging
			common.SysLog("[upstream-audit] " + kind + " recorded: sent=" + ev.SentModel +
				" got=" + ev.ResponseModel + " src=" + src + " " + note)
		}
	}()
}
