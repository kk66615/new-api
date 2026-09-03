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
	return false
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
// this, so treat it as a screening signal, not proof.
func InferUpstreamSource(channelType int, headersJSON, respId, respModel string) string {
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
	if prev == "" || prev == src {
		return "", false
	}
	return prev, true
}

// appendUpstreamResponseAudit adds the ursp block to other.admin_info and
// persists forensic evidence asynchronously. EVERY observed response is
// persisted (kind=normal for clean ones — user decision 2026-09-02, full
// retention for evidence completeness; normal rows are pruned after
// model.NormalEvidenceRetentionDays, anomalies are kept forever).
func appendUpstreamResponseAudit(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, adminInfo map[string]interface{}) {
	if relayInfo == nil || adminInfo == nil {
		return
	}
	respModel := relayInfo.UpstreamResponseModel
	if respModel == "" {
		return // tri-state: upstream never declared a model — record nothing
	}
	sentModel := relayInfo.UpstreamSentModel
	if sentModel == "" {
		sentModel = relayInfo.UpstreamModelName
	}
	mismatch := !UpstreamModelsMatchForAudit(sentModel, respModel)

	channelId, channelType := 0, 0
	if relayInfo.ChannelMeta != nil {
		channelId = relayInfo.ChannelMeta.ChannelId
		channelType = relayInfo.ChannelMeta.ChannelType
	}
	src := InferUpstreamSource(channelType, relayInfo.UpstreamResponseHeaders, relayInfo.UpstreamResponseId, respModel)

	ursp := map[string]interface{}{
		"model":    respModel,
		"sent":     sentModel,
		"mismatch": mismatch,
		"src":      src,
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
	adminInfo["ursp"] = ursp

	from, flipped := trackSourceFlip(channelId, src)
	if flipped {
		ursp["src_flip"] = from + "->" + src
	}

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
