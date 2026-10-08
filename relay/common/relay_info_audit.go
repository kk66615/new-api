package common

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"

	"github.com/tidwall/gjson"
)

// ── upstream response audit: per-attempt observation state ─────────────────
// Everything here is pure observation (上游响应审计，纯旁路观测): nothing in this
// file may change what is forwarded, billed or returned to the client.

const (
	// UpstreamExcerptCap bounds the stored response excerpt in bytes after
	// sanitising: 65535 is the MySQL TEXT limit.
	UpstreamExcerptCap = 65535
	// UpstreamFrameCap bounds the stored model-bearing and error frames.
	UpstreamFrameCap = 16 * 1024
	// upstreamHeadersCap bounds the stored header JSON.
	upstreamHeadersCap = 8 * 1024
	// promptHashMaxBytes: bodies up to this size are read in full at the send
	// boundary and get a prompt hash. Larger bodies (long contexts, inline
	// images, possibly disk-backed) are never read back in full.
	promptHashMaxBytes = 1 << 20
	// wireModelPrefixBytes: for larger or unknown-size bodies only this
	// prefix is read, to find the "model" field.
	wireModelPrefixBytes = 64 << 10
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
	// NoModelConcept marks responses that have no model to judge (images):
	// evidence is kept, but the observation level stays "none".
	NoModelConcept bool

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

	// OutputSHA256 / OutputChars describe the normalised natural-language
	// output (content + reasoning, no tool calls: agents legitimately repeat
	// identical tool calls); only the hash is kept.
	OutputSHA256 string
	OutputChars  int
	// outputText accumulates streamed output text until FinishUpstreamOutput
	// (a byte slice, not a strings.Builder, so RelayInfo stays copyable).
	outputText []byte

	// CompletionTokens as settled, for the generation-speed signal.
	CompletionTokens int

	// BodyMarkers accumulates vendor-specific response body fields seen in
	// ANY observed frame of this attempt (Azure puts prompt_filter_results in
	// the first chat chunk and content_filter_results in content chunks, so
	// the last frame alone is not enough).
	BodyMarkers UpstreamBodyMarkers
}

// UpstreamBodyMarkers is a bitset of vendor fingerprints found in response
// bodies. Only fields with documented or production evidence are listed.
type UpstreamBodyMarkers uint8

const (
	// BodyMarkerAzureContentFilter: Azure content-safety annotations
	// (prompt_filter_results / content_filter_results / Responses
	// content_filters). Azure-platform specific, whatever the model.
	BodyMarkerAzureContentFilter UpstreamBodyMarkers = 1 << iota
	// BodyMarkerBedrockMetrics: amazon-bedrock-invocationMetrics, attached
	// by Bedrock to the final stream event.
	BodyMarkerBedrockMetrics
	// BodyMarkerDeepSeekCacheHit / Miss: the official DeepSeek API reports
	// prompt_cache_hit_tokens and prompt_cache_miss_tokens; both are needed,
	// and only in an official-shaped usage object (isDeepSeekShapedUsage).
	BodyMarkerDeepSeekCacheHit
	BodyMarkerDeepSeekCacheMiss
	// BodyMarkerNewAPICacheSplit: claude_cache_creation_5_m_tokens, written
	// into usage by a NewAPI hop. Evidence about the hop, never the backend.
	BodyMarkerNewAPICacheSplit
)

// upstreamBodyMarkerNeedles pairs a cheap pre-filter (bytes.Contains on the
// quoted key) with a confirmation at the protocol-defined path: the same
// words can appear in positions nobody but the client controls — model text
// (escaped quotes), tool arguments, or a Responses event echoing the
// request's tools / metadata / text.format schema.
var upstreamBodyMarkerNeedles = []struct {
	needle  []byte
	marker  UpstreamBodyMarkers
	confirm func(frame []byte) bool
}{
	// chat completion object / chunk: top-level only
	{[]byte(`"prompt_filter_results"`), BodyMarkerAzureContentFilter, func(f []byte) bool {
		return gjson.GetBytes(f, "prompt_filter_results").IsArray()
	}},
	// chat choices, chat prompt annotations (prompt_annotations is the first
	// chunk's key in Asynchronous Filter mode), or entries of the Responses
	// content_filters array
	{[]byte(`"content_filter_results"`), BodyMarkerAzureContentFilter, func(f []byte) bool {
		return anyGJSONMatch(f, "choices.#.content_filter_results", "prompt_annotations.#.content_filter_results",
			"content_filters.#.content_filter_results", "response.content_filters.#.content_filter_results")
	}},
	// Responses object (non-stream) or event (stream): top-level of the response only
	{[]byte(`"content_filters"`), BodyMarkerAzureContentFilter, func(f []byte) bool {
		return gjson.GetBytes(f, "content_filters").IsArray() || gjson.GetBytes(f, "response.content_filters").IsArray()
	}},
	// Bedrock stream event: a top-level object on the event itself
	{[]byte(`"amazon-bedrock-invocationMetrics"`), BodyMarkerBedrockMetrics, func(f []byte) bool {
		return gjson.GetBytes(f, "amazon-bedrock-invocationMetrics").IsObject()
	}},
	// official DeepSeek usage (chat completion / final chunk)
	{[]byte(`"prompt_cache_hit_tokens"`), BodyMarkerDeepSeekCacheHit, func(f []byte) bool {
		return gjson.GetBytes(f, "usage.prompt_cache_hit_tokens").Type == gjson.Number && isDeepSeekShapedUsage(f)
	}},
	{[]byte(`"prompt_cache_miss_tokens"`), BodyMarkerDeepSeekCacheMiss, func(f []byte) bool {
		return gjson.GetBytes(f, "usage.prompt_cache_miss_tokens").Type == gjson.Number && isDeepSeekShapedUsage(f)
	}},
	// NewAPI hop usage (chat usage, or a Responses object's usage)
	{[]byte(`"claude_cache_creation_5_m_tokens"`), BodyMarkerNewAPICacheSplit, func(f []byte) bool {
		return gjson.GetBytes(f, "usage.claude_cache_creation_5_m_tokens").Type == gjson.Number ||
			gjson.GetBytes(f, "response.usage.claude_cache_creation_5_m_tokens").Type == gjson.Number
	}},
}

// Official usage key sets (fetched 2026-10-08):
//   - DeepSeek, api-docs.deepseek.com "Create Chat Completion": usage is the
//     five OpenAI keys plus prompt_cache_hit_tokens / prompt_cache_miss_tokens;
//     prompt_tokens_details carries only cached_tokens and
//     completion_tokens_details only reasoning_tokens.
//   - OpenAI CompletionUsage, developers.openai.com chat completion object
//     and streaming events: the detail objects below. They are tolerated
//     because a pass-through hop may add OpenAI-standard detail subfields.
var (
	deepSeekUsageKeys = []string{
		"prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details",
		"completion_tokens_details", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens",
	}
	openAIPromptDetailKeys     = []string{"audio_tokens", "cache_write_tokens", "cached_tokens", "image_tokens", "text_tokens"}
	openAICompletionDetailKeys = []string{"accepted_prediction_tokens", "audio_tokens", "reasoning_tokens", "rejected_prediction_tokens", "text_tokens"}
)

// isDeepSeekShapedUsage reports whether the frame's usage object has only
// keys the official DeepSeek (or OpenAI-standard detail) schema defines.
// Relays that rewrite usage into a union of several vendors' fields
// (cache_read_input_tokens, prompt_cache_write_tokens, credit, cached_tokens
// at the top level, ...) also carry the DeepSeek cache keys, so those keys
// alone do not point at DeepSeek.
func isDeepSeekShapedUsage(frame []byte) bool {
	usage := gjson.GetBytes(frame, "usage")
	if !usage.IsObject() {
		return false
	}
	pure := true
	usage.ForEach(func(key, value gjson.Result) bool {
		var allowedDetails []string
		switch k := key.String(); {
		case !slices.Contains(deepSeekUsageKeys, k):
			pure = false
		case k == "prompt_tokens_details":
			allowedDetails = openAIPromptDetailKeys
		case k == "completion_tokens_details":
			allowedDetails = openAICompletionDetailKeys
		}
		switch {
		case !pure || allowedDetails == nil || value.Type == gjson.Null:
		case !value.IsObject():
			pure = false // a details value that is neither an object nor null is off-schema
		default:
			value.ForEach(func(detailKey, _ gjson.Result) bool {
				pure = slices.Contains(allowedDetails, detailKey.String())
				return pure
			})
		}
		return pure
	})
	return pure
}

// anyGJSONMatch reports whether any of the #-query paths matches at least
// one element.
func anyGJSONMatch(frame []byte, paths ...string) bool {
	for _, path := range paths {
		if len(gjson.GetBytes(frame, path).Array()) > 0 {
			return true
		}
	}
	return false
}

// Has reports whether every marker in want is present.
func (m UpstreamBodyMarkers) Has(want UpstreamBodyMarkers) bool {
	return m&want == want
}

// ObserveUpstreamBodyMarkers scans one raw response frame or body (before any
// truncation) for vendor fingerprints. The byte pre-filter keeps the common
// case to a few bytes.Contains; only a hit is parsed, and needles whose bit
// is already set are skipped.
func (info *RelayInfo) ObserveUpstreamBodyMarkers(frame []byte) {
	if info == nil || len(frame) == 0 {
		return
	}
	for _, n := range upstreamBodyMarkerNeedles {
		if info.UpstreamAudit.BodyMarkers&n.marker == 0 && bytes.Contains(frame, n.needle) && n.confirm(frame) {
			info.UpstreamAudit.BodyMarkers |= n.marker
		}
	}
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
// is not what the upstream received. Bodies of known size up to
// promptHashMaxBytes are read in full and also yield the prompt hash and an
// explicit temperature; anything larger (or of unknown size) is inspected
// through a 64KB prefix only, so the extra memory per attempt stays bounded.
// Write-once per attempt.
func (info *RelayInfo) SnapshotUpstreamRequest(req *http.Request) {
	if info == nil || info.UpstreamSentModel != "" {
		return
	}
	var body []byte
	complete := false
	if req != nil && req.GetBody != nil && !strings.HasPrefix(strings.ToLower(req.Header.Get("Content-Type")), "multipart/") {
		if rc, err := req.GetBody(); err == nil && rc != nil {
			limit := int64(wireModelPrefixBytes)
			if req.ContentLength > 0 && req.ContentLength <= promptHashMaxBytes {
				limit = req.ContentLength
			}
			body, _ = io.ReadAll(io.LimitReader(rc, limit+1))
			_ = rc.Close()
			complete = int64(len(body)) <= limit && req.ContentLength > 0 && int64(len(body)) == req.ContentLength
		}
	}
	wire := ""
	if v := gjson.GetBytes(body, "model"); v.Type == gjson.String {
		wire = strings.TrimSpace(v.String())
	}
	if wire == "" && req != nil && req.URL != nil {
		wire = modelFromUpstreamPath(req.URL.Path)
	}
	if !complete {
		body = nil // never hash a partial prompt
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
	if len(body) == 0 || len(body) > promptHashMaxBytes {
		return // hashing re-encodes the prompt; keep that bounded
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

// canonicalJSONModifier re-emits a JSON value with object keys sorted and all
// insignificant whitespace removed, without decoding it into Go values.
const canonicalJSONModifier = `@pretty:{"sortKeys":true}|@ugly`

// CanonicalPromptSHA256 hashes the prompt-bearing fields of a request body in
// canonical form (object keys sorted, whitespace removed), so key order and
// formatting do not change the hash. The canonical form is produced on the
// raw bytes rather than by decoding into maps, keeping large prompts cheap.
// Returns "" when the body carries none of the fields.
func CanonicalPromptSHA256(body []byte) string {
	h := sha256.New()
	found := false
	for _, field := range promptHashFields {
		v := gjson.GetBytes(body, field+"|"+canonicalJSONModifier)
		if !v.Exists() {
			continue
		}
		found = true
		h.Write([]byte(field))
		h.Write([]byte{':'})
		h.Write([]byte(v.Raw))
		h.Write([]byte{'\n'})
	}
	if !found {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
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
		// bounded like the DB column: the id also keys the in-memory replay memory
		info.UpstreamResponseId = common.TruncateAuditRunes(responseId, 255)
	}
	if created != 0 {
		info.UpstreamResponseCreated = created
	}
	if systemFingerprint != "" {
		info.UpstreamSystemFingerprint = common.TruncateAuditRunes(systemFingerprint, 128) // bounded like the DB column
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
	info.ObserveUpstreamBodyMarkers(common.StringToByteSlice(frame))
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

// ObserveUpstreamOutput records a hash of the normalised output text
// (whitespace collapsed) and its length in characters. Callers pass
// natural-language output only — content and reasoning, never tool calls.
// The text itself is not kept.
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

// upstreamOutputTextCap bounds the streamed output text kept for hashing.
const upstreamOutputTextCap = 1 << 20

// AppendUpstreamOutputText accumulates streamed output text (content and
// reasoning only) for the output hash; FinishUpstreamOutput hashes it.
func (info *RelayInfo) AppendUpstreamOutputText(text string) {
	if info == nil || text == "" || len(info.UpstreamAudit.outputText) >= upstreamOutputTextCap {
		return
	}
	info.UpstreamAudit.outputText = append(info.UpstreamAudit.outputText, text[:min(len(text), upstreamOutputTextCap-len(info.UpstreamAudit.outputText))]...)
}

// FinishUpstreamOutput hashes the text gathered by AppendUpstreamOutputText.
func (info *RelayInfo) FinishUpstreamOutput() {
	if info == nil {
		return
	}
	info.ObserveUpstreamOutput(string(info.UpstreamAudit.outputText))
	info.UpstreamAudit.outputText = nil
}

// ObserveUpstreamCompletionTokens records the settled completion token count
// for the generation-speed signal.
func (info *RelayInfo) ObserveUpstreamCompletionTokens(completionTokens int) {
	if info == nil {
		return
	}
	info.UpstreamAudit.CompletionTokens = completionTokens
}

// UpstreamObservationLevel is ursp.obs: "full" when the upstream declared a
// model, "partial" when the exchange was observed but declared none, "none"
// when no instrumented handler looked at it.
func (info *RelayInfo) UpstreamObservationLevel() string {
	switch {
	case info == nil, info.UpstreamAudit.NoModelConcept:
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
		info.ObserveUpstreamBodyMarkers(common.StringToByteSlice(excerpt))
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
		info.ObserveUpstreamBodyMarkers(body) // the whole body, not just the stored prefix
		head := body[:min(len(body), UpstreamExcerptCap+utf8.UTFMax)]
		var cut bool
		info.UpstreamResponseExcerpt, cut = common.TruncateAuditBytes(string(head), UpstreamExcerptCap)
		// a short body can still be cut: invalid bytes grow on replacement
		info.UpstreamAudit.ExcerptTruncated = cut || len(body) > UpstreamExcerptCap
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
	slices.Sort(keys)
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

// upstreamOutputPaths locate the generated natural-language output (text and
// reasoning, not tool calls) in the non-stream response shapes of OpenAI
// chat/completions, Responses, Claude and Gemini. The paths do not collide
// across protocols, so one list serves all of them.
var upstreamOutputPaths = []string{
	"choices.#.message.content", "choices.#.message.reasoning_content", "choices.#.message.reasoning",
	"choices.#.text",
	"output.#.content.#.text", "output.#.summary.#.text",
	"content.#.text", "content.#.thinking",
	"candidates.#.content.parts.#.text",
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

// upstreamMediaExcerptCap bounds the excerpt of a media response: the head
// carries created/ids/revised prompts, the rest is base64 payload.
const upstreamMediaExcerptCap = 4 * 1024

// ObserveUpstreamMediaResponse keeps cheap evidence for responses without a
// model concept (image generation): the headers and the first 4KB of the body
// with its full length. The observation level stays "none".
func (info *RelayInfo) ObserveUpstreamMediaResponse(handler string, body []byte, headers map[string][]string) {
	if info == nil {
		return
	}
	info.MarkUpstreamObserver(handler)
	info.UpstreamAudit.NoModelConcept = true
	head := body[:min(len(body), upstreamMediaExcerptCap+utf8.UTFMax)]
	info.SetUpstreamResponseForensicsBytes(head, headers)
	if len(body) > 0 {
		var cut bool
		info.UpstreamResponseExcerpt, cut = common.TruncateAuditBytes(info.UpstreamResponseExcerpt, upstreamMediaExcerptCap)
		info.UpstreamAudit.ExcerptTruncated = cut || len(body) > upstreamMediaExcerptCap
		info.UpstreamAudit.ExcerptFullLen = len(body)
	}
	info.ObserveUpstreamResponse("", "", gjson.GetBytes(head, "created").Int(), "")
}
