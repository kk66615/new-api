package common

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"

	"github.com/tidwall/gjson"
)

// ── upstream response audit: per-attempt observation state ─────────────────
// Everything here is pure observation (上游响应审计，纯旁路观测): nothing in this
// file may change what is forwarded, billed or returned to the client.

const (
	// UpstreamExcerptCap bounds the stored response excerpt.
	UpstreamExcerptCap = 64 * 1024
	// UpstreamFrameCap bounds the stored model-bearing and error frames.
	UpstreamFrameCap = 16 * 1024
	// upstreamHeadersCap bounds the stored header JSON.
	upstreamHeadersCap = 8 * 1024
	// wireInspectMaxBytes bounds how much of an outgoing body the send-boundary
	// snapshot reads. Larger bodies (image-heavy prompts) still yield their
	// model when it precedes the cut, but get no prompt hash.
	wireInspectMaxBytes = 8 << 20
	// maxObservedModels bounds the distinct-model set kept for one stream.
	maxObservedModels = 8
	// HeadersTruncatedKey marks header JSON that hit upstreamHeadersCap.
	HeadersTruncatedKey = "_headers_truncated"
)

// UpstreamAuditObservation is the audit state added on top of the original
// Upstream* fields of RelayInfo. It belongs to one upstream attempt and is
// cleared by ResetUpstreamObservation.
type UpstreamAuditObservation struct {
	// Handler names the response handler that observed the exchange; empty
	// when no instrumented handler ran (the log then reports obs=none).
	Handler string
	// Observed is set by any observation call, so "the upstream declared
	// nothing" (partial) is distinguishable from "nobody looked" (none).
	Observed bool

	// SentField is UpstreamModelName at the send boundary when it differs
	// from the model actually on the wire (pass-through bodies, param
	// override rewriting "model").
	SentField string
	// PromptSHA256 hashes the canonicalised prompt of the body actually sent
	// (messages/input/contents + system/instructions + tools); no text kept.
	PromptSHA256 string
	// Temperature is the explicit temperature of the sent body, nil if absent.
	Temperature *float64

	// FirstModel / Models: first non-empty model and the distinct set, in
	// order of appearance (bounded). The last one is UpstreamResponseModel.
	FirstModel string
	Models     []string
	// ModelFrame is the first model-bearing stream frame (or the tail of an
	// oversized non-stream body); ErrorFrame the upstream error frame.
	ModelFrame string
	ErrorFrame string

	ExcerptTruncated bool
	ExcerptFullLen   int
	HeadersTruncated bool

	// OutputSHA256 / OutputChars describe the normalised output text
	// (content + reasoning + tool calls); no text kept.
	OutputSHA256 string
	OutputChars  int

	// Token counts as settled, for the speed and prompt-ratio signals.
	PromptTokens     int
	CompletionTokens int
	TokensLocal      bool
}

// SnapshotUpstreamSentModel freezes UpstreamModelName as the sent model. It is
// the fallback for send paths that have no HTTP request to inspect (WebSocket
// dials); HTTP sends use SnapshotUpstreamRequest, which reads the wire.
//
// The window matters in both directions:
//   - too early (e.g. in ModelMappedHelper) and it captures a pre-strip name
//     like "claude-3-7-sonnet-thinking", so an upstream that honestly echoes
//     the request is recorded as having swapped the model;
//   - too late (after a response handler has run) and it adopts the model the
//     UPSTREAM declared, making every comparison compare-equal and turning the
//     audit into a silent no-op.
//
// The value is therefore write-once, and InitChannelMeta clears it per attempt
// (a retry can land on a different channel with a different model mapping).
func (info *RelayInfo) SnapshotUpstreamSentModel() {
	if info == nil || info.UpstreamSentModel != "" {
		return
	}
	if info.ChannelMeta != nil {
		info.UpstreamSentModel = info.UpstreamModelName
	}
}

// SnapshotUpstreamRequest freezes the model that is really on the wire, read
// from the final outgoing request at the send boundary: the JSON body "model"
// when present, otherwise the model segment of a Gemini/Vertex/Bedrock URL,
// otherwise UpstreamModelName. This covers pass-through bodies with model
// mapping and param overrides that rewrite "model", where UpstreamModelName
// is not what the upstream received. It also hashes the sent prompt and
// records an explicit temperature. Write-once per attempt.
func (info *RelayInfo) SnapshotUpstreamRequest(req *http.Request) {
	if info == nil || info.UpstreamSentModel != "" {
		return
	}
	var body []byte
	if req != nil && req.GetBody != nil && !strings.HasPrefix(strings.ToLower(req.Header.Get("Content-Type")), "multipart/") {
		if rc, err := req.GetBody(); err == nil && rc != nil {
			body, _ = io.ReadAll(io.LimitReader(rc, wireInspectMaxBytes+1))
			_ = rc.Close()
		}
	}
	wire := ""
	if v := gjson.GetBytes(body, "model"); v.Type == gjson.String {
		wire = strings.TrimSpace(v.String())
	}
	if wire == "" && req != nil && req.URL != nil {
		wire = modelFromUpstreamPath(req.URL.Path)
	}
	if len(body) > wireInspectMaxBytes {
		body = nil // incomplete: never hash a partial prompt
	}
	info.SnapshotUpstreamWire(wire, body)
}

// SnapshotUpstreamWire records a wire model and the sent body for send paths
// that do not go through an *http.Request (the AWS SDK passes the Bedrock
// modelId and InvokeModel body). An empty wire model falls back to
// UpstreamModelName. Write-once per attempt.
func (info *RelayInfo) SnapshotUpstreamWire(wireModel string, body []byte) {
	if info == nil || info.UpstreamSentModel != "" {
		return
	}
	field := ""
	if info.ChannelMeta != nil {
		field = info.UpstreamModelName
	}
	if wireModel == "" {
		wireModel = field
	}
	info.UpstreamSentModel = common.TruncateAuditRunes(wireModel, 255)
	if field != "" && field != wireModel {
		info.UpstreamAudit.SentField = common.TruncateAuditRunes(field, 255)
	}
	if len(body) == 0 {
		return
	}
	info.UpstreamAudit.PromptSHA256 = CanonicalPromptSHA256(body)
	for _, path := range []string{"temperature", "generationConfig.temperature", "generation_config.temperature"} {
		if v := gjson.GetBytes(body, path); v.Type == gjson.Number {
			t := v.Float()
			info.UpstreamAudit.Temperature = &t
			break
		}
	}
}

var (
	// .../models/gemini-2.5-pro:generateContent (Gemini, Vertex publishers path)
	upstreamPathModelRe = regexp.MustCompile(`/models/([^/:]+)(?::[A-Za-z]+)?/?$`)
	// .../model/anthropic.claude-...-v1:0/invoke (Bedrock HTTP API)
	upstreamBedrockPathRe = regexp.MustCompile(`/model/([^/]+)/(?:invoke|invoke-with-response-stream|converse|converse-stream)/?$`)
)

func modelFromUpstreamPath(path string) string {
	if m := upstreamBedrockPathRe.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	if m := upstreamPathModelRe.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return ""
}

// promptHashFields are the request fields that make up "the prompt" across
// the OpenAI chat/completions/Responses, Claude and Gemini request shapes.
var promptHashFields = []string{
	"messages", "input", "prompt", "contents",
	"system", "instructions", "systemInstruction", "system_instruction",
	"tools",
}

// CanonicalPromptSHA256 hashes the prompt-bearing fields of a request body in
// canonical form (re-encoded with sorted object keys, so key order and
// whitespace do not change the hash). Returns "" when the body carries none.
func CanonicalPromptSHA256(body []byte) string {
	canonical := make(map[string]any)
	for _, field := range promptHashFields {
		v := gjson.GetBytes(body, field)
		if !v.Exists() {
			continue
		}
		var parsed any
		if err := common.UnmarshalJsonStr(v.Raw, &parsed); err != nil {
			parsed = v.Raw
		}
		canonical[field] = parsed
	}
	if len(canonical) == 0 {
		return ""
	}
	encoded, err := common.Marshal(canonical)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// ResetUpstreamObservation clears everything observed from the previous
// attempt's upstream response (sent-model snapshot, declared model/id,
// forensics, usage) so the next attempt starts clean. Called from
// InitChannelMeta, which runs once per upstream attempt. Without this, a
// failed attempt's declared model would be attributed to the channel that
// served the retry whenever that retry's response omits the model.
func (info *RelayInfo) ResetUpstreamObservation() {
	if info == nil {
		return
	}
	info.UpstreamSentModel = ""
	info.UpstreamResponseModel = ""
	info.UpstreamResponseId = ""
	info.UpstreamResponseCreated = 0
	info.UpstreamSystemFingerprint = ""
	info.UpstreamResponseExcerpt = ""
	info.UpstreamResponseHeaders = ""
	info.UpstreamInputTokens = 0
	info.UpstreamCacheCreationTokens = 0
	info.UpstreamCacheReadTokens = 0
	info.UpstreamAudit = UpstreamAuditObservation{}
}

// MarkUpstreamObserver names the response handler observing this attempt
// (ursp.h). The first name wins, so a wrapper (e.g. the AWS adaptor) can
// label an exchange before delegating to a shared handler.
func (info *RelayInfo) MarkUpstreamObserver(handler string) {
	if info == nil || info.UpstreamAudit.Handler != "" {
		return
	}
	info.UpstreamAudit.Handler = handler
}

// ObserveUpstreamResponse records the model identity the upstream response
// declared. Call it at the point the response is parsed, BEFORE any code path
// that rewrites info.UpstreamModelName (e.g. claude message_start handling).
// The response fields are updated on every call so streaming paths naturally
// keep the last declaration; the first model and the distinct set are kept
// alongside so a stream that changes model mid-way is visible.
// Empty model is a no-op for the model (tri-state: absent stays absent).
func (info *RelayInfo) ObserveUpstreamResponse(model, responseId string, created int64, systemFingerprint string) {
	if info == nil {
		return
	}
	info.UpstreamAudit.Observed = true
	model = common.TruncateAuditRunes(strings.TrimSpace(model), 200)
	// Fallback snapshot for paths that never reach the send boundary
	// (tests, task relays). UpstreamModelName is promoted from the embedded
	// *ChannelMeta, which those paths may leave nil.
	if info.UpstreamSentModel == "" && info.ChannelMeta != nil {
		info.UpstreamSentModel = info.UpstreamModelName
	}
	// a terminal event may omit the model (seen on converted relays); keep the
	// id/created/fingerprint so the exchange is still recorded (model-less)
	if model != "" {
		info.UpstreamResponseModel = model
		audit := &info.UpstreamAudit
		if audit.FirstModel == "" {
			audit.FirstModel = model
		}
		if !slices.Contains(audit.Models, model) && len(audit.Models) < maxObservedModels {
			audit.Models = append(audit.Models, model)
		}
	}
	if responseId != "" {
		info.UpstreamResponseId = responseId
	}
	if created != 0 {
		info.UpstreamResponseCreated = created
	}
	if systemFingerprint != "" {
		info.UpstreamSystemFingerprint = systemFingerprint
	}
}

// ObserveUpstreamResponseFrame is ObserveUpstreamResponse for one stream
// frame: the first frame that declares a model is kept (UTF-8 safe, 16KB) so
// the evidence always contains the frame the verdict is based on, even when
// the stream ends with a model-less usage frame.
func (info *RelayInfo) ObserveUpstreamResponseFrame(frame, model, responseId string, created int64, systemFingerprint string) {
	if info == nil {
		return
	}
	info.ObserveUpstreamResponse(model, responseId, created, systemFingerprint)
	if strings.TrimSpace(model) != "" && info.UpstreamAudit.ModelFrame == "" {
		info.UpstreamAudit.ModelFrame, _ = common.TruncateAuditBytes(frame, UpstreamFrameCap)
	}
}

// ObserveUpstreamErrorFrame keeps an upstream error frame separately from the
// excerpt, so an in-stream error never overwrites the frame that declared the
// model.
func (info *RelayInfo) ObserveUpstreamErrorFrame(frame string) {
	if info == nil || frame == "" {
		return
	}
	info.UpstreamAudit.Observed = true
	info.UpstreamAudit.ErrorFrame, _ = common.TruncateAuditBytes(frame, UpstreamFrameCap)
}

// ObserveUpstreamUsage records the upstream's own input-side token accounting.
// Zero values are ignored so a later, fuller declaration (message_delta) never
// erases an earlier one.
func (info *RelayInfo) ObserveUpstreamUsage(inputTokens, cacheCreation, cacheRead int) {
	if info == nil {
		return
	}
	if inputTokens > 0 {
		info.UpstreamInputTokens = inputTokens
	}
	if cacheCreation > 0 {
		info.UpstreamCacheCreationTokens = cacheCreation
	}
	if cacheRead > 0 {
		info.UpstreamCacheReadTokens = cacheRead
	}
}

// ObserveUpstreamOutput records a hash of the normalised output (whitespace
// collapsed) and its length in characters. The text itself is not kept.
func (info *RelayInfo) ObserveUpstreamOutput(text string) {
	if info == nil {
		return
	}
	normalized := strings.Join(strings.Fields(text), " ")
	if normalized == "" {
		return
	}
	sum := sha256.Sum256([]byte(normalized))
	info.UpstreamAudit.OutputSHA256 = hex.EncodeToString(sum[:])
	info.UpstreamAudit.OutputChars = utf8.RuneCountInString(normalized)
}

// ObserveUpstreamTokenCounts records the settled prompt (all input incl.
// cache) and completion token counts; local marks counts we estimated
// ourselves because the upstream reported none.
func (info *RelayInfo) ObserveUpstreamTokenCounts(promptTokens, completionTokens int, local bool) {
	if info == nil {
		return
	}
	info.UpstreamAudit.PromptTokens = promptTokens
	info.UpstreamAudit.CompletionTokens = completionTokens
	info.UpstreamAudit.TokensLocal = local
}

// UpstreamObservationLevel is ursp.obs: "full" when the upstream declared a
// model, "partial" when the exchange was observed but declared none, "none"
// when no instrumented handler looked at it.
func (info *RelayInfo) UpstreamObservationLevel() string {
	switch {
	case info == nil:
		return "none"
	case info.UpstreamResponseModel != "":
		return "full"
	case info.UpstreamAudit.Observed:
		return "partial"
	default:
		return "none"
	}
}

// SetUpstreamResponseForensics stores the raw evidence material: the excerpt
// is cut to UpstreamExcerptCap on a rune boundary with NUL bytes and invalid
// UTF-8 removed (the full length is kept), and the headers are redacted and
// capped. An empty excerpt is a headers-only update.
func (info *RelayInfo) SetUpstreamResponseForensics(excerpt string, headers map[string][]string) {
	if info == nil {
		return
	}
	if excerpt != "" {
		info.UpstreamAudit.Observed = true
		full := len(excerpt)
		info.UpstreamResponseExcerpt, info.UpstreamAudit.ExcerptTruncated = common.TruncateAuditBytes(excerpt, UpstreamExcerptCap)
		info.UpstreamAudit.ExcerptFullLen = full
	}
	if headers != nil {
		info.UpstreamAudit.Observed = true
		info.UpstreamResponseHeaders, info.UpstreamAudit.HeadersTruncated = EncodeUpstreamAuditHeaders(headers)
	}
}

// SetUpstreamResponseForensicsBytes is SetUpstreamResponseForensics for a raw
// body: only the stored prefix is converted to a string, never the whole body.
// For a body larger than the excerpt cap, its tail is kept as the model frame
// (Gemini puts modelVersion after the candidates, beyond a 64KB prefix).
func (info *RelayInfo) SetUpstreamResponseForensicsBytes(body []byte, headers map[string][]string) {
	if info == nil {
		return
	}
	if len(body) > 0 {
		info.UpstreamAudit.Observed = true
		head := body[:min(len(body), UpstreamExcerptCap+utf8.UTFMax)]
		info.UpstreamResponseExcerpt, _ = common.TruncateAuditBytes(string(head), UpstreamExcerptCap)
		info.UpstreamAudit.ExcerptTruncated = len(body) > UpstreamExcerptCap
		info.UpstreamAudit.ExcerptFullLen = len(body)
		if len(body) > UpstreamExcerptCap && info.UpstreamAudit.ModelFrame == "" {
			tail := body[max(0, len(body)-UpstreamFrameCap-utf8.UTFMax):]
			info.UpstreamAudit.ModelFrame = common.TruncateAuditBytesTail(string(tail), UpstreamFrameCap)
		}
	}
	if headers != nil {
		info.UpstreamAudit.Observed = true
		info.UpstreamResponseHeaders, info.UpstreamAudit.HeadersTruncated = EncodeUpstreamAuditHeaders(headers)
	}
}

// redactedAuditHeaders never store their values: they carry credentials.
var redactedAuditHeaders = map[string]bool{
	"set-cookie": true, "cookie": true, "authorization": true,
	"proxy-authorization": true, "x-api-key": true, "api-key": true,
}

// EncodeUpstreamAuditHeaders serialises response headers as a JSON object of
// strings, keys sorted. Every value of a key is kept (joined with ", "),
// credential-bearing headers are replaced with "[redacted]", and the output
// is capped at 8KB: headers that no longer fit are skipped and the
// HeadersTruncatedKey marker is added instead of dropping everything.
func EncodeUpstreamAuditHeaders(headers map[string][]string) (string, bool) {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	marker := `"` + HeadersTruncatedKey + `":"true"`
	var b strings.Builder
	b.WriteByte('{')
	truncated := false
	written := 0
	for _, k := range keys {
		values := headers[k]
		if len(values) == 0 {
			continue
		}
		value := strings.Join(values, ", ")
		if redactedAuditHeaders[strings.ToLower(k)] {
			value = "[redacted]"
		}
		kb, errK := common.Marshal(common.SanitizeAuditText(k))
		vb, errV := common.Marshal(common.SanitizeAuditText(value))
		if errK != nil || errV != nil {
			continue
		}
		pairLen := len(kb) + 1 + len(vb)
		// reserve room for a separator, the marker and the closing brace
		if b.Len()+1+pairLen+1+len(marker)+1 > upstreamHeadersCap {
			truncated = true
			continue
		}
		if written > 0 {
			b.WriteByte(',')
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
		written++
	}
	if truncated {
		if written > 0 {
			b.WriteByte(',')
		}
		b.WriteString(marker)
	}
	b.WriteByte('}')
	return b.String(), truncated
}

// upstreamOutputPaths locate the generated output in the non-stream response
// shapes of OpenAI chat/completions, Responses, Claude and Gemini. The paths
// do not collide across protocols, so one list serves all of them.
var upstreamOutputPaths = []string{
	"choices.#.message.content", "choices.#.message.reasoning_content", "choices.#.message.reasoning",
	"choices.#.message.tool_calls.#.function.name", "choices.#.message.tool_calls.#.function.arguments",
	"choices.#.text",
	"output.#.content.#.text", "output.#.summary.#.text", "output.#.name", "output.#.arguments",
	"content.#.text", "content.#.thinking", "content.#.name", "content.#.input",
	"candidates.#.content.parts.#.text", "candidates.#.content.parts.#.functionCall",
}

// ObserveUpstreamOutputJSON records the output hash of a complete JSON
// response (see ObserveUpstreamOutput) without keeping any text.
func (info *RelayInfo) ObserveUpstreamOutputJSON(body []byte) {
	if info == nil || len(body) == 0 {
		return
	}
	var b strings.Builder
	var collect func(v gjson.Result)
	collect = func(v gjson.Result) {
		switch {
		case v.IsArray():
			v.ForEach(func(_, item gjson.Result) bool {
				collect(item)
				return true
			})
		case v.Type == gjson.String:
			b.WriteString(v.String())
			b.WriteByte(' ')
		case v.Exists() && v.Type != gjson.Null:
			b.WriteString(v.Raw)
			b.WriteByte(' ')
		}
	}
	for _, path := range upstreamOutputPaths {
		collect(gjson.GetBytes(body, path))
	}
	info.ObserveUpstreamOutput(b.String())
}

// ObserveUpstreamResponsesEvent observes one Responses API stream event that
// carries a response object: every such event declares the model (the first
// supplies the model frame), and a terminal event — success or not — becomes
// the excerpt and supplies the full output for the output hash.
func (info *RelayInfo) ObserveUpstreamResponsesEvent(eventType, frame, model, responseId string, createdAt int64) {
	if info == nil {
		return
	}
	info.ObserveUpstreamResponseFrame(frame, model, responseId, createdAt, "")
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		info.SetUpstreamResponseForensics(frame, nil)
		info.ObserveUpstreamOutputJSON([]byte(gjson.Get(frame, "response").Raw))
	}
}
