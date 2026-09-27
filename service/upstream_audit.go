package service

import (
	"encoding/json"
	"regexp"
	"strconv"
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

// snapshotAliasVerdict classifies the vendor alias↔pinned-build pattern: one
// name extends the other by a strict date segment set (gpt-5 vs
// gpt-5-2025-08-07, x vs x-0731, claude-x vs claude-x-20251001), by a Gemini
// "-NNN" version (gemini-2.0-flash vs gemini-2.0-flash-001) or by "-latest"
// (claude-sonnet-4-6 vs claude-sonnet-4-6-latest). It returns "" when the pair
// does not match this pattern. Deliberately NOT a general prefix match — an
// arbitrary suffix (deepseek-v4 vs deepseek-v4-flash, deepseek-v3 vs
// deepseek-v3-1-250821, gpt-5 vs gpt-5-1234) is a different model and must
// still flag.
//
// Direction matters for pinned builds: sending the alias and getting a dated
// build back is the vendor resolving its alias (snapshot_date /
// version_suffix). Sending a pinned build and getting the bare alias back means
// the upstream did not honour the pin — it may be serving any build — so that
// direction is pinned_to_alias, which counts as a mismatch.
func snapshotAliasVerdict(sent, resp string) string {
	sent, resp = strings.ToLower(strings.TrimSpace(sent)), strings.ToLower(strings.TrimSpace(resp))
	alias, pinned, sentIsAlias := sent, resp, true
	if len(sent) > len(resp) {
		alias, pinned, sentIsAlias = resp, sent, false
	}
	suffix, ok := strings.CutPrefix(pinned, alias+"-")
	if !ok {
		return ""
	}
	if suffix == "latest" {
		return VerdictRollingAlias
	}
	verdict := ""
	switch {
	case isSnapshotDateSuffix(suffix):
		verdict = VerdictSnapshotDate
	case strings.HasPrefix(alias, "gemini-") && isThreeDigitVersion(suffix):
		verdict = VerdictVersionSuffix
	default:
		return ""
	}
	if !sentIsAlias {
		return VerdictPinnedToAlias
	}
	return verdict
}

// isSnapshotDateSuffix accepts only the date forms vendors actually use for
// pinned builds, with the month and day range-checked so an arbitrary number
// (gpt-5-1234, deepseek-v3-1-250821) is never mistaken for a date:
//
//	YYYY-MM-DD  OpenAI      gpt-5-2025-08-07
//	YYYYMMDD    Anthropic   claude-haiku-4-5-20251001
//	YYMMDD      Volcengine  deepseek-v3-250324
//	MMDD        DeepSeek    deepseek-v4-flash-0731, gpt-4-0613
//	MM-DD       Google      gemini-2.5-flash-preview-05-20
func isSnapshotDateSuffix(suffix string) bool {
	segs := strings.Split(suffix, "-")
	for _, seg := range segs {
		if !isAllDigits(seg) {
			return false
		}
	}
	switch len(segs) {
	case 3:
		return len(segs[0]) == 4 && strings.HasPrefix(segs[0], "20") &&
			len(segs[1]) == 2 && len(segs[2]) == 2 && validMonthDay(segs[1], segs[2])
	case 2:
		return len(segs[0]) == 2 && len(segs[1]) == 2 && validMonthDay(segs[0], segs[1])
	case 1:
		d := segs[0]
		switch len(d) {
		case 8:
			return strings.HasPrefix(d, "20") && validMonthDay(d[4:6], d[6:8])
		case 6:
			return validMonthDay(d[2:4], d[4:6])
		case 4:
			return validMonthDay(d[0:2], d[2:4])
		}
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isThreeDigitVersion(s string) bool {
	return len(s) == 3 && isAllDigits(s)
}

func validMonthDay(mm, dd string) bool {
	m, errM := strconv.Atoi(mm)
	d, errD := strconv.Atoi(dd)
	return errM == nil && errD == nil && m >= 1 && m <= 12 && d >= 1 && d <= 31
}

// bedrockModelRe matches Bedrock model ids: an optional region/routing prefix
// (global. / us. / eu. / apac. ...), a vendor segment and the model name, e.g.
// global.anthropic.claude-sonnet-5, anthropic.claude-haiku-4-5-20251001-v1:0.
var bedrockModelRe = regexp.MustCompile(`^(?:[a-z]{2,6}\.)?(anthropic|amazon|meta|mistral|cohere|ai21|deepseek|writer|stability|luma|twelvelabs|qwen|openai)\.(.+)$`)
var bedrockVersionSuffixRe = regexp.MustCompile(`-v\d+(?::\d+)?$`)
var colonVersionSuffixRe = regexp.MustCompile(`-v\d+:\d+$`)

func isBedrockModelId(m string) bool {
	return bedrockModelRe.MatchString(strings.ToLower(strings.TrimSpace(m)))
}

// splitVendorPrefix separates a vendor namespace from the bare model name:
// OpenRouter-style "vendor/model" and Bedrock-style "region.vendor.model"
// (deepseek/deepseek-v4-flash → "deepseek" + deepseek-v4-flash,
// global.anthropic.claude-sonnet-5 → "global.anthropic" + claude-sonnet-5).
// The Bedrock "-vN:M" build suffix is NOT touched here: whether it takes part
// in the comparison depends on the other side (see canonicalModelPair).
func splitVendorPrefix(m string) (prefix, bare string, bedrock bool) {
	bare = strings.TrimSpace(m)
	if i := strings.LastIndex(bare, "/"); i >= 0 && i < len(bare)-1 {
		prefix, bare = bare[:i], bare[i+1:]
	}
	if loc := bedrockModelRe.FindStringSubmatchIndex(strings.ToLower(bare)); loc != nil {
		modelStart := loc[4]
		bedrockPrefix := bare[:modelStart-1]
		if prefix != "" {
			prefix += "/" + bedrockPrefix
		} else {
			prefix = bedrockPrefix
		}
		bare = bare[modelStart:]
		bedrock = true
	}
	return prefix, bare, bedrock
}

// canonicalModelPair reduces both names to comparable bare ids and returns the
// namespace prefix the response carried (recorded as ursp.prefix and fed into
// the backend tracker, so a relay that starts answering "deepseek/…" for an
// OpenAI model is visible even though the comparison ignores the prefix).
//
// The Bedrock "-vN[:M]" build suffix is compared when BOTH sides carry one
// (anthropic.x-v1:0 vs anthropic.x-v2:0 is a different build). When only one
// side has it — Bedrock bodies report the vendor model name, and clients send
// the bare name — it is stripped from that side. A "-vN" suffix is only
// treated as a Bedrock build marker on a Bedrock-shaped id or in the
// unambiguous "-vN:M" form, never on names like deepseek-v3.
func canonicalModelPair(rawSent, rawResp string) (sent, resp, respPrefix string) {
	_, sent, sentBedrock := splitVendorPrefix(rawSent)
	respPrefix, resp, respBedrock := splitVendorPrefix(rawResp)
	sentVersioned := (sentBedrock && bedrockVersionSuffixRe.MatchString(sent)) || colonVersionSuffixRe.MatchString(sent)
	respVersioned := (respBedrock && bedrockVersionSuffixRe.MatchString(resp)) || colonVersionSuffixRe.MatchString(resp)
	if sentVersioned && respVersioned {
		return sent, resp, respPrefix
	}
	if sentVersioned {
		sent = bedrockVersionSuffixRe.ReplaceAllString(sent, "")
	}
	if respVersioned {
		resp = bedrockVersionSuffixRe.ReplaceAllString(resp, "")
	}
	return sent, resp, respPrefix
}

// Mismatch verdict types. VerdictMismatch and VerdictPinnedToAlias are
// anomalies (see VerdictIsMismatch); every other value means "consistent",
// with the reason retained so the UI can label a benign difference honestly
// instead of showing a bare boolean.
const (
	VerdictExact         = "exact"           // identical after canonicalization
	VerdictVendorPrefix  = "vendor_prefix"   // differed only by a vendor/namespace prefix
	VerdictRuntimeAlias  = "runtime_alias"   // vendor alias ↔ runtime build id (grok-4.5 ↔ grok-4.5-build)
	VerdictSnapshotDate  = "snapshot_date"   // sent alias, got dated snapshot (gpt-5 → gpt-5-2025-08-07)
	VerdictVersionSuffix = "version_suffix"  // sent Gemini alias, got its -NNN build (gemini-2.0-flash → gemini-2.0-flash-001)
	VerdictRollingAlias  = "rolling_alias"   // "-latest" rolling alias resolving into the same family and tier
	VerdictPreviewAlias  = "preview_alias"   // "-preview" public name ↔ the bare id the upstream reports it as
	VerdictNoModel       = "no_model"        // upstream declared no model at all
	VerdictPinnedToAlias = "pinned_to_alias" // sent a pinned build, got the bare alias back — pin not honoured (mismatch)
	VerdictMismatch      = "mismatch"        // different model — the anomaly we exist to catch
)

// VerdictIsMismatch reports whether a verdict is an anomaly. It is the only
// place that decides which verdicts count as a mismatch.
func VerdictIsMismatch(verdict string) bool {
	return verdict == VerdictMismatch || verdict == VerdictPinnedToAlias
}

// ClassifyUpstreamModelMismatch returns the verdict for the pair, explaining
// WHY it is (not) a mismatch. It is the single decision point: both the audit
// boolean and the log/UI label come from here, so they can never disagree.
func ClassifyUpstreamModelMismatch(sentModel, responseModel string) string {
	rawSent := strings.TrimSpace(sentModel)
	rawResp := strings.TrimSpace(responseModel)
	if rawSent == "" || rawResp == "" {
		return VerdictNoModel // never flag on missing data
	}
	sent, resp, _ := canonicalModelPair(rawSent, rawResp)
	if strings.EqualFold(sent, resp) {
		if !strings.EqualFold(rawSent, rawResp) {
			return VerdictVendorPrefix
		}
		return VerdictExact
	}
	if g := canonicalGrokRuntimeModel(sent); g != "" && g == canonicalGrokRuntimeModel(resp) {
		return VerdictRuntimeAlias
	}
	if v := snapshotAliasVerdict(sent, resp); v != "" {
		return v
	}
	if latestFamilyMatch(sent, resp) {
		return VerdictRollingAlias
	}
	if previewAliasMatch(sent, resp) {
		return VerdictPreviewAlias
	}
	return VerdictMismatch
}

// previewAliasMatch accepts the one direction in which a "-preview" suffix is
// a naming convention rather than a tier change: the public model name carries
// "-preview" and the upstream reports the same id without it (Google image
// models: gemini-3-pro-image-preview → modelVersion gemini-3-pro-image,
// user decision 2026-09-19 after 2540 identical rows on one channel). Scoped
// to the gemini family: for OpenAI the bare name is a different product
// (o1-preview vs o1, gpt-4.5-preview vs gpt-4.5) and must stay a mismatch.
// The reverse (sent bare, got -preview) is deliberately NOT accepted — that
// is the upstream substituting a different build.
func previewAliasMatch(sent, resp string) bool {
	const suffix = "-preview"
	if len(sent) <= len(suffix) || !strings.EqualFold(sent[len(sent)-len(suffix):], suffix) {
		return false
	}
	base := sent[:len(sent)-len(suffix)]
	if !strings.HasPrefix(strings.ToLower(base), "gemini-") {
		return false
	}
	return strings.EqualFold(base, resp)
}

// UpstreamModelsMatchForAudit reports whether the upstream-declared model is
// consistent with the model we sent. Case-insensitive; vendor/ prefixes, known
// vendor runtime aliases and alias→snapshot date suffixes are canonicalized
// before comparing.
func UpstreamModelsMatchForAudit(sentModel, responseModel string) bool {
	return !VerdictIsMismatch(ClassifyUpstreamModelMismatch(sentModel, responseModel))
}

// lowerTierTokens are the qualifier tokens vendors use for a cheaper, weaker
// sibling of the same family. Compared as whole "-"-separated tokens, never as
// substrings. "turbo" is deliberately absent: gpt-4 → gpt-4-turbo is a newer
// build of the same grade, not a downgrade.
var lowerTierTokens = map[string]bool{
	"flash": true, "mini": true, "lite": true, "nano": true, "small": true,
	"preview": true, "fast": true, "air": true, "instant": true, "haiku": true,
}

// higherTierRank ranks the qualifiers that mark a bigger tier, so a
// replacement can be told apart from an upgrade: qwen-max → qwen-plus and
// claude-opus → claude-sonnet are downgrades, qwen-plus → qwen-max is not.
var higherTierRank = map[string]int{
	"ultra": 4, "max": 4, "opus": 4,
	"pro":  3,
	"plus": 2, "sonnet": 2,
	"thinking": 1, "reasoning": 1,
}

// modelTokens splits a canonical (prefix-stripped, lowercased) model id into
// its family word and the remaining tokens. The family is the leading letters
// of the first token (gpt, claude, gemini, qwen from qwen3, o from o3); an id
// that starts with a digit — the #57 case, where an upstream answered
// "3.1-flash-lite-preview-03-2026" for gemini-3.7-flash — has no family, and
// its tokens are compared against the sent model's non-family tokens.
func modelTokens(m string) (family string, tokens []string) {
	parts := strings.Split(m, "-")
	first := parts[0]
	i := 0
	for i < len(first) && first[i] >= 'a' && first[i] <= 'z' {
		i++
	}
	if i == 0 {
		return "", parts
	}
	family = first[:i]
	tokens = parts[1:]
	if rest := first[i:]; rest != "" {
		tokens = append([]string{rest}, tokens...)
	}
	return family, tokens
}

// sameModelFamily reports whether two canonical ids can be compared tier-wise:
// same family word, or a response that carries no family word at all.
func sameModelFamily(sent, resp string) (sentTokens, respTokens []string, ok bool) {
	sentFamily, sentTokens := modelTokens(sent)
	respFamily, respTokens := modelTokens(resp)
	if respFamily != "" && sentFamily != respFamily {
		return nil, nil, false
	}
	return sentTokens, respTokens, true
}

// UpstreamModelIsTierDowngrade reports whether the upstream served a weaker
// sibling of the model family we asked for. It is a LABEL ONLY: it never feeds
// the mismatch verdict (appendUpstreamResponseAudit gates it on mismatch), so a
// false positive here can mislabel a log but can never change what is flagged.
//
// Recognised shapes, all within one family (or against a response that lost
// its family word):
//   - a lower-tier word appears that we did not send — additive (gpt-5 →
//     gpt-5-mini) and replacement (pro → flash, flash → flash-lite,
//     sonnet → haiku, 4o → 4o-mini) alike. A date riding along
//     (gpt-5-preview-2025-08-07) does not upgrade the grade.
//   - a higher-tier word we sent is gone and nothing ranked as high replaced
//     it (gpt-5-pro → gpt-5, opus → sonnet, max → plus, x-thinking → x);
//     plus → max is an upgrade and is not labelled.
//
// A pure version change is reported separately (UpstreamModelIsVersionDowngrade).
func UpstreamModelIsTierDowngrade(sentModel, responseModel string) bool {
	sent, resp, _ := canonicalModelPair(sentModel, responseModel)
	sent, resp = strings.ToLower(sent), strings.ToLower(resp)
	if sent == "" || resp == "" || sent == resp {
		return false
	}
	// An alias/snapshot/namespace relationship is a naming difference, not a
	// tier swap — never label those as downgrades.
	if !VerdictIsMismatch(ClassifyUpstreamModelMismatch(sentModel, responseModel)) {
		return false
	}
	sentTokens, respTokens, ok := sameModelFamily(sent, resp)
	if !ok {
		return false
	}
	sentSet := make(map[string]bool, len(sentTokens))
	for _, tok := range sentTokens {
		sentSet[tok] = true
	}
	respSet := make(map[string]bool, len(respTokens))
	newRank := 0 // best higher-tier word the response introduced
	for _, tok := range respTokens {
		if lowerTierTokens[tok] && !sentSet[tok] {
			return true
		}
		respSet[tok] = true
		if !sentSet[tok] {
			newRank = max(newRank, higherTierRank[tok])
		}
	}
	for _, tok := range sentTokens {
		if rank := higherTierRank[tok]; rank > 0 && !respSet[tok] && newRank < rank {
			return true // a higher-tier word we sent is gone and nothing as high replaced it
		}
	}
	return false
}

// modelVersion extracts the numeric model version (major, minor) from the
// non-family tokens: the first token shaped like 3, 3.7, v4 or 4o, plus a
// following 1–2 digit token as the minor (claude-sonnet-4-5 → 4.5). Tokens of
// three or more digits are builds or dates, never versions.
func modelVersion(tokens []string) (major, minor int, ok bool) {
	for i, tok := range tokens {
		m := modelVersionTokenRe.FindStringSubmatch(tok)
		if m == nil {
			continue
		}
		major, _ = strconv.Atoi(m[1])
		if m[2] != "" {
			minor, _ = strconv.Atoi(m[2])
		} else if i+1 < len(tokens) && len(tokens[i+1]) <= 2 && isAllDigits(tokens[i+1]) {
			minor, _ = strconv.Atoi(tokens[i+1])
		}
		return major, minor, true
	}
	return 0, 0, false
}

var modelVersionTokenRe = regexp.MustCompile(`^v?(\d{1,2})(?:\.(\d{1,2}))?[a-z]*$`)

// UpstreamModelIsVersionDowngrade reports whether the response declares an
// older numeric version of the same family than we sent (gemini-3.7-flash →
// 3.1-flash-lite-…, claude-opus-4-7 → claude-opus-4-5). Recorded separately
// from the tier label and, like it, only meaningful on a mismatch.
func UpstreamModelIsVersionDowngrade(sentModel, responseModel string) bool {
	if !VerdictIsMismatch(ClassifyUpstreamModelMismatch(sentModel, responseModel)) {
		return false
	}
	sent, resp, _ := canonicalModelPair(sentModel, responseModel)
	sentTokens, respTokens, ok := sameModelFamily(strings.ToLower(sent), strings.ToLower(resp))
	if !ok {
		return false
	}
	sMajor, sMinor, sOk := modelVersion(sentTokens)
	rMajor, rMinor, rOk := modelVersion(respTokens)
	if !sOk || !rOk {
		return false
	}
	return rMajor < sMajor || (rMajor == sMajor && rMinor < sMinor)
}

// latestFamilyMatch accepts a vendor "-latest" rolling alias resolving to a
// concrete version of the SAME family and tier (gemini-flash-latest →
// gemini-3.7-flash): every non-version token of the alias (minus "latest")
// must appear in the response id, and the response must not carry a
// lower-tier word the alias does not name. gemini-flash-latest →
// gemini-3.1-pro still flags ("flash" missing), and gemini-flash-latest →
// gemini-3.7-flash-lite flags ("lite" was never asked for).
func latestFamilyMatch(sentModel, responseModel string) bool {
	sent := strings.ToLower(sentModel)
	alias, ok := strings.CutSuffix(sent, "-latest")
	if !ok {
		return false
	}
	aliasTokens := map[string]bool{}
	for t := range strings.SplitSeq(alias, "-") {
		aliasTokens[t] = true
	}
	respTokens := map[string]bool{}
	for t := range strings.SplitSeq(strings.ToLower(responseModel), "-") {
		if lowerTierTokens[t] && !aliasTokens[t] {
			return false
		}
		respTokens[t] = true
	}
	for t := range aliasTokens {
		if t != "" && !respTokens[t] {
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
	return inferBackendShape(channelType, id, modelLower, ccmaxHint)
}

// officialSources are labels InferUpstreamSource only assigns from the channel
// protocol or from vendor response headers — evidence about who actually
// served the response, not about the wire format.
var officialSources = map[string]bool{
	"aws": true, "vertex": true, "azure": true, "google": true, "aistudio": true,
	"anthropic": true, "openai": true, "kimi": true,
}

// neutralShapes are the inferBackendShape fallbacks that carry no positive
// evidence of a specific backend: an OpenAI- or Anthropic-shaped body could
// have come from the vendor, from Azure, or from any relay.
var neutralShapes = map[string]bool{"gpt_relay": true, "claude_relay": true, "unknown": true}

// reconcileBackendWithSource returns the backend label to display. Positive
// wire-shape evidence (msg_bdrk_, gen-, deepseek/glm/kimi naming, vertex ids)
// always wins. When the shape is neutral but the source was established from
// official vendor headers (e.g. Azure's apim-request-id / x-ms-region), the
// backend follows the source instead of falling back to "gpt_relay", which
// reads as an accusation the body never supported (user question 2026-09-20:
// "src is azure, why is the backend gpt_relay?"). The flip tracker keeps
// watching the raw shape so this never creates or hides a shape switch.
func reconcileBackendWithSource(backendShape, src string) string {
	if neutralShapes[backendShape] && officialSources[src] {
		return src
	}
	return backendShape
}

// inferBackendShape classifies a response by wire shape alone (id prefix +
// model naming), ignoring headers and channel type. It is the "look through
// the relay" view: a NewAPI relay that returns msg_bdrk_ ids or Bedrock model
// names is fronting AWS Bedrock, whatever its own headers say.
func inferBackendShape(channelType int, id, respModel string, ccmaxHint bool) string {
	modelLower := strings.ToLower(respModel)
	switch {
	case strings.HasPrefix(id, "msg_bdrk_"), isBedrockModelId(respModel):
		return "aws"
	case strings.HasPrefix(id, "msg_vrtx_"), strings.HasPrefix(id, "req_vrtx_"):
		return "vertex"
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

var backendTracker sync.Map // channelId(int) -> *channelSrcState (look-through backend shape)

func trackSourceFlip(channelId int, src string) (from string, flipped bool) {
	return trackFlip(&srcTracker, channelId, src)
}

// trackBackendFlip watches the wire-shape backend behind a channel independently
// of the hop label, so a relay that silently moves from Bedrock to a Claude
// relay is caught even though its own headers never change.
func trackBackendFlip(channelId int, backend string) (from string, flipped bool) {
	return trackFlip(&backendTracker, channelId, backend)
}

func trackFlip(tracker *sync.Map, channelId int, src string) (from string, flipped bool) {
	if channelId == 0 || src == "" || src == "unknown" {
		return "", false
	}
	v, _ := tracker.LoadOrStore(channelId, &channelSrcState{})
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

// AppendUpstreamResponseAuditForError is the entry point for the error-log
// path (controller/relay.go processChannelError), which builds its own
// LogOther and writes it through RecordErrorLog. Without it, failed exchanges
// carried no ursp at all — backwards, since a channel that fails is the one an
// audit most wants evidence on. errorType labels the record (ursp.err and the
// evidence note) so a failed attempt is never mistaken for a clean exchange.
//
// It is a no-op when nothing was observed, so calling it on a path that has no
// upstream response (quota rejections, model-not-found before dispatch) is free.
func AppendUpstreamResponseAuditForError(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, other *model.LogOther, errorType string) {
	if errorType == "" {
		errorType = "unknown"
	}
	appendUpstreamResponseAudit(ctx, relayInfo, other, errorType)
}

// appendUpstreamResponseAudit adds the ursp block to other.root_info and
// persists forensic evidence asynchronously. EVERY observed response is
// persisted (kind=normal for clean ones — user decision 2026-09-02, full
// retention for evidence completeness; normal rows are pruned after
// model.NormalEvidenceRetentionDays, anomalies are kept forever).
//
// errorType is empty on the success path. When set, the attempt failed: the
// record is labelled (ursp.err, evidence note) and the source/backend flip
// trackers are NOT fed — they are calibrated on success shapes, and a relay
// that answers errors with its own headers would otherwise flip on every error
// and flip back on the next success.
func appendUpstreamResponseAudit(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, other *model.LogOther, errorType string) {
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
	// verdict is the single decision point: the boolean below and the label the
	// UI renders both derive from it, so they can never disagree.
	verdict := ClassifyUpstreamModelMismatch(sentModel, respModel)
	mismatch := VerdictIsMismatch(verdict)
	tierDowngrade := mismatch && UpstreamModelIsTierDowngrade(sentModel, respModel)
	versionDowngrade := mismatch && UpstreamModelIsVersionDowngrade(sentModel, respModel)
	_, _, respPrefix := canonicalModelPair(sentModel, respModel)

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
	// backendShape is the wire-shape-only view (what the flip tracker watches);
	// backend is what gets displayed: when the shape is neutral and the source
	// carries official-vendor evidence, the displayed backend follows the source
	backendShape := inferBackendShape(channelType, relayInfo.UpstreamResponseId, respModel, ccmaxHint)
	backend := reconcileBackendWithSource(backendShape, src)

	ursp := map[string]interface{}{
		"sent":     sentModel,
		"mismatch": mismatch,
		"kind":     verdict, // why it matched/mismatched (exact / snapshot_date / mismatch / ...)
		"src":      src,
	}
	if tierDowngrade {
		// the highest-signal adulteration shape: upstream served a weaker
		// sibling of the same family (…-flash / …-mini / …-preview)
		ursp["tier_downgrade"] = true
	}
	if versionDowngrade {
		ursp["version_downgrade"] = true // older numeric version of the family (3.7 → 3.1)
	}
	if respPrefix != "" {
		ursp["prefix"] = respPrefix // vendor namespace stripped before comparing (deepseek/, global.anthropic.)
	}
	// Channel TYPE only. The numeric channel id is deliberately NOT duplicated
	// here: the log record already carries it (log.channel_id / channel_name),
	// and both are read from the same relayInfo.ChannelMeta at the same moment,
	// so a second copy could only ever drift. The type adds information the
	// record lacks — it is what tells "we configured an AWS channel" apart from
	// "the wire shape just looked like AWS".
	if channelType != 0 {
		ursp["ch_type"] = channelType
	}
	// mapping layer: present only when the channel redirected the model, so an
	// intentional mapping is never confused with an upstream swap
	if relayInfo.IsModelMapped {
		ursp["mapped"] = true
	}
	if backend != "unknown" {
		ursp["backend"] = backend // wire-shape backend behind the hop (aws / vertex / claude_relay / ...)
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
	failed := errorType != ""
	if failed {
		ursp["err"] = errorType // this attempt failed; the observation is of its error response
	}
	var from, bfrom string
	var flipped, bflipped bool
	if !failed {
		from, flipped = trackSourceFlip(channelId, src)
		if flipped {
			ursp["src_flip"] = from + "->" + src
		}
		bfrom, bflipped = trackBackendFlip(channelId, backendShape)
		if bflipped {
			// both sides are raw shapes: the flip string describes the wire
			// shape change only; the reconciled label lives in ursp.backend
			ursp["backend_flip"] = bfrom + "->" + backendShape
		}
	}
	flipNote := ""
	if flipped {
		flipNote = from + " -> " + src
	}
	if bflipped {
		if flipNote != "" {
			flipNote += "; "
		}
		flipNote += "backend: " + bfrom + " -> " + backendShape
	}
	flipped = flipped || bflipped
	// root-only projection: LogOther strips root_info for admin and user
	// readers alike (user decision 2026-09-19: upstream-declared model data
	// is for the super administrator only)
	other.SetRoot("ursp", ursp)

	kind, note := "normal", ""
	switch {
	case flipped && !mismatch:
		kind = "provider_switch"
		note = flipNote
	case mismatch && flipped:
		kind = "model_mismatch"
		note = "provider also flipped: " + flipNote
	case mismatch:
		kind = "model_mismatch"
	}
	if failed {
		if note != "" {
			note += "; "
		}
		note += "error: " + errorType
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
