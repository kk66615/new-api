package service

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/setting/audit_setting"

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
// version_suffix). Sending a pinned build and getting the bare alias back is
// labelled pinned_to_alias: recorded and shown, but not a mismatch (official
// DeepSeek answers deepseek-v4-pro-0813 as deepseek-v4-pro).
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
//	MM-YYYY     Google      gemini-2.5-flash-preview-09-2025
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
		if len(segs[0]) == 2 && len(segs[1]) == 4 { // Google MM-YYYY: gemini-2.5-flash-preview-09-2025
			return strings.HasPrefix(segs[1], "20") && validMonthDay(segs[0], "01")
		}
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

// vertexAtVersionRe matches the Vertex AI "@YYYYMMDD" pin (and the "-vN@"
// revision form): the wire carries claude-sonnet-4-5@20250929 or
// claude-3-5-sonnet-v2@20241022 while the Claude body declares
// claude-sonnet-4-5-20250929 / claude-3-5-sonnet-20241022.
var vertexAtVersionRe = regexp.MustCompile(`(?:-v\d+)?@(\d{8})$`)

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
	sent, resp = vertexAtVersionRe.ReplaceAllString(sent, "-$1"), vertexAtVersionRe.ReplaceAllString(resp, "-$1")
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

// Mismatch verdict types. Only VerdictMismatch is an anomaly (see
// VerdictIsMismatch); every other value means "consistent",
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
	VerdictPinnedToAlias = "pinned_to_alias" // sent a pinned build, got the bare alias back — recorded, not a mismatch (owner decision 2026-09-28: official DeepSeek does this)
	VerdictMismatch      = "mismatch"        // different model — the anomaly we exist to catch
)

// VerdictIsMismatch reports whether a verdict is an anomaly. It is the only
// place that decides which verdicts count as a mismatch.
func VerdictIsMismatch(verdict string) bool {
	return verdict == VerdictMismatch
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
		// "preview" is how Google ships the build a gemini -latest alias
		// points at (gemini-flash-latest → gemini-2.5-flash-preview-09-2025);
		// for gemini it stays a tier label but does not fail the resolution
		if lowerTierTokens[t] && !(t == "preview" && strings.HasPrefix(alias, "gemini-")) && !aliasTokens[t] {
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
	if err := common.UnmarshalJsonStr(headersJSON, &m); err != nil {
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

// ── source / backend flip tracking ──────────────────────────────────────────
// A channel whose inferred source flips (az → gpt_relay → az ...) is a strong
// adulteration / backend-swap signal. EVERY known→known flip is recorded as
// evidence (kind=provider_switch) — no rate limiting, per user decision
// 2026-09-02: evidence completeness beats table size. Push-side dedup in the
// watchdog keeps notifications sane. State is in-memory: after a restart the
// first request re-seeds silently (no false alarm).
//
// State is keyed by (channel, model family, multi-key index), not by channel:
// a multi-model channel legitimately routes claude and gpt to different
// backends, and each key of a multi-key channel may be a different account,
// so comparing across them produced provider_switch rows that were not
// switches at all.

type flipTrackerKey struct {
	channelId int
	family    string
	keyIndex  int
}

type channelSrcState struct {
	mu   sync.Mutex
	last string
}

var srcTracker sync.Map     // flipTrackerKey -> *channelSrcState
var backendTracker sync.Map // flipTrackerKey -> *channelSrcState (look-through backend shape + vendor prefix)

// auditTrackerKey derives the tracker key for one exchange.
func auditTrackerKey(relayInfo *relaycommon.RelayInfo, sentModel string) flipTrackerKey {
	key := flipTrackerKey{family: auditModelFamily(sentModel)}
	if relayInfo != nil && relayInfo.ChannelMeta != nil {
		key.channelId = relayInfo.ChannelId
		if relayInfo.ChannelIsMultiKey {
			key.keyIndex = relayInfo.ChannelMultiKeyIndex
		}
	}
	return key
}

// auditModelFamily is the family word of a model id (gpt, claude, gemini,
// deepseek, qwen, o …), used to partition the flip trackers.
func auditModelFamily(model string) string {
	_, bare, _ := splitVendorPrefix(model)
	family, tokens := modelTokens(strings.ToLower(bare))
	if family == "" && len(tokens) > 0 {
		return tokens[0]
	}
	return family
}

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

func trackSourceFlip(key flipTrackerKey, src string) (from string, flipped bool) {
	return trackFlip(&srcTracker, key, src)
}

// trackBackendFlip watches the wire-shape backend behind a channel independently
// of the hop label, so a relay that silently moves from Bedrock to a Claude
// relay is caught even though its own headers never change. The signature
// includes the vendor prefix the response carried (gpt_relay/deepseek), so a
// relay that starts answering through a different namespace is caught too.
//
// Only a change of wire shape, or of vendor between two responses that both
// carry one, is a flip: a relay alternating gpt-4o / openai/gpt-4o, or Bedrock
// region prefixes (us. / global.), is the same backend. A response without a
// vendor keeps the last vendor seen, so v1 → none → v2 is still caught.
func trackBackendFlip(key flipTrackerKey, backend string) (from string, flipped bool) {
	if key.channelId == 0 || backend == "" || backend == "unknown" {
		return "", false
	}
	v, _ := backendTracker.LoadOrStore(key, &channelSrcState{})
	st := v.(*channelSrcState)
	st.mu.Lock()
	defer st.mu.Unlock()
	prev := st.last
	if prev == "" {
		st.last = backend
		return "", false
	}
	prevShape, prevVendor, _ := strings.Cut(prev, "/")
	shape, vendor, _ := strings.Cut(backend, "/")
	if srcFamily(prevShape) == srcFamily(shape) && (vendor == "" || prevVendor == "" || vendor == prevVendor) {
		if vendor != "" || prevVendor == "" {
			st.last = backend
		}
		return "", false
	}
	st.last = backend
	return prev, true
}

func trackFlip(tracker *sync.Map, key flipTrackerKey, src string) (from string, flipped bool) {
	if key.channelId == 0 || src == "" || src == "unknown" {
		return "", false
	}
	v, _ := tracker.LoadOrStore(key, &channelSrcState{})
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

// backendSignature is what the backend tracker compares: the wire shape plus
// the vendor segment of the response's prefix (openrouter/deepseek → deepseek,
// us.anthropic → anthropic). Gemini's "models/" path prefix is not a vendor.
func backendSignature(backendShape, prefix string) string {
	vendor := strings.ToLower(prefix)
	if i := strings.LastIndex(vendor, "/"); i >= 0 {
		vendor = vendor[i+1:]
	}
	if i := strings.LastIndex(vendor, "."); i >= 0 {
		vendor = vendor[i+1:]
	}
	if backendShape == "" || backendShape == "unknown" || vendor == "" || vendor == "models" {
		return backendShape
	}
	return backendShape + "/" + vendor
}

// ── ccmax (Claude Max subscription relay) detection ──────────────────────────
// Such relays strip every Anthropic header, but they MUST inject the Claude Code
// OAuth system prompt (~3-4k tokens) to be accepted upstream. The upstream then
// reports far more input (incl. cache) than we estimated for the client's own
// request. A request that shows the injection is labelled directly. The
// channel only becomes sticky — so requests whose own big cached prompts hide
// the injection still get the label — after two visible injections within an
// hour, and the stickiness expires 24h after the last visible injection.

type ccmaxState struct {
	mu      sync.Mutex
	lastHit time.Time
	hits    int
	sticky  bool
}

var ccmaxChannels sync.Map // channelId(int) -> *ccmaxState

const (
	ccmaxHitWindow  = time.Hour
	ccmaxStickyTTL  = 24 * time.Hour
	ccmaxStickyHits = 2
)

// auditNow is the audit clock; tests replace it.
var auditNow = time.Now

// ccmaxObserve feeds one exchange into the channel's ccmax state (unless
// record is false, e.g. for channel tests) and returns the hint for it.
func ccmaxObserve(channelId int, injected, record bool) bool {
	if channelId == 0 {
		return injected
	}
	v, _ := ccmaxChannels.LoadOrStore(channelId, &ccmaxState{})
	st := v.(*ccmaxState)
	st.mu.Lock()
	defer st.mu.Unlock()
	now := auditNow()
	if st.sticky && now.Sub(st.lastHit) > ccmaxStickyTTL {
		st.sticky, st.hits = false, 0
	}
	if injected {
		if record {
			if !st.lastHit.IsZero() && now.Sub(st.lastHit) <= ccmaxHitWindow {
				st.hits++
			} else {
				st.hits = 1
			}
			st.lastHit = now
			if st.hits >= ccmaxStickyHits {
				st.sticky = true
			}
		}
		return true
	}
	return st.sticky
}

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
// On a path with no upstream response (quota rejections, model-not-found
// before dispatch) it only records obs=none; no evidence row is written.
func AppendUpstreamResponseAuditForError(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, other *model.LogOther, errorType string) {
	if errorType == "" {
		errorType = "unknown"
	}
	appendUpstreamResponseAudit(ctx, relayInfo, other, errorType)
}

// appendUnobservedUpstreamAudit marks a log whose path has no upstream model
// to judge (tasks, Midjourney) as obs=none with the path name, so its absence
// of a verdict is explicit rather than silent.
func appendUnobservedUpstreamAudit(other *model.LogOther, handler string) {
	other.SetRoot("ursp", map[string]any{"obs": "none", "h": handler})
}

// upstreamObserverName is ursp.h: the response handler that observed the
// exchange, or — for paths without an instrumented handler — the relay mode.
func upstreamObserverName(relayInfo *relaycommon.RelayInfo) string {
	if relayInfo.UpstreamAudit.Handler != "" {
		return relayInfo.UpstreamAudit.Handler
	}
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		return "image"
	case relayconstant.RelayModeAudioSpeech, relayconstant.RelayModeAudioTranscription, relayconstant.RelayModeAudioTranslation:
		return "audio"
	case relayconstant.RelayModeRerank:
		return "rerank"
	case relayconstant.RelayModeRealtime:
		return "realtime"
	case relayconstant.RelayModeEmbeddings:
		return "embedding"
	case relayconstant.RelayModeModerations:
		return "moderation"
	}
	return "unobserved"
}

// appendUpstreamResponseAudit adds the ursp block to other.root_info and
// queues the forensic evidence row. EVERY observed response is persisted
// (kind=normal for clean ones — user decision 2026-09-02; normal rows are
// pruned after model.NormalEvidenceRetentionDays, every other kind is kept).
// A text log with nothing observed still gets ursp.obs=none and ursp.h, so
// "not observed" is distinguishable from "the upstream declared nothing".
//
// errorType is empty on the success path. When set, the attempt failed: the
// record is labelled (ursp.err, evidence note) and no tracker is fed — they
// are calibrated on success shapes, and a relay that answers errors with its
// own headers would otherwise flip on every error and flip back on the next
// success. Channel tests (F2) never feed trackers either and are labelled
// channel_test in the evidence note.
func appendUpstreamResponseAudit(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, other *model.LogOther, errorType string) {
	if relayInfo == nil || other == nil {
		return
	}
	audit := &relayInfo.UpstreamAudit
	failed := errorType != ""
	sentModel := relayInfo.UpstreamSentModel
	if sentModel == "" && relayInfo.ChannelMeta != nil {
		sentModel = relayInfo.UpstreamModelName
	}
	ursp := map[string]any{
		"obs": relayInfo.UpstreamObservationLevel(),
		"h":   upstreamObserverName(relayInfo),
	}
	if failed {
		ursp["err"] = errorType // this attempt failed; the observation is of its error response
	}
	respModel := relayInfo.UpstreamResponseModel
	if respModel == "" && relayInfo.UpstreamResponseId == "" && relayInfo.UpstreamResponseExcerpt == "" {
		// nothing to judge: record only that it was not observed
		if sentModel != "" {
			ursp["sent"] = sentModel
		}
		other.SetRoot("ursp", ursp)
		return
	}
	// respModel may be empty: the upstream sent a terminal event without a model.
	// Tri-state still applies to the mismatch verdict (never flags), but the
	// exchange is recorded (ursp.nomodel + evidence) so it stays auditable.

	// verdict is the single decision point: the boolean below and the label the
	// UI renders both derive from it, so they can never disagree.
	verdict := ClassifyUpstreamModelMismatch(sentModel, respModel)
	mismatch := VerdictIsMismatch(verdict)
	tierDowngrade := mismatch && UpstreamModelIsTierDowngrade(sentModel, respModel)
	versionDowngrade := mismatch && UpstreamModelIsVersionDowngrade(sentModel, respModel)
	_, _, respPrefix := canonicalModelPair(sentModel, respModel)
	// a stream changed model when any two of its declarations are not benign
	// renames of each other (alias → dated snapshot, vendor prefix, …)
	midStreamChange := false
	for i := 0; i < len(audit.Models) && !midStreamChange; i++ {
		for _, later := range audit.Models[i+1:] {
			if VerdictIsMismatch(ClassifyUpstreamModelMismatch(audit.Models[i], later)) {
				midStreamChange = true
				break
			}
		}
	}

	channelId, channelType := 0, 0
	if relayInfo.ChannelMeta != nil {
		channelId = relayInfo.ChannelMeta.ChannelId
		channelType = relayInfo.ChannelMeta.ChannelType
	}
	channelTest := relayInfo.IsChannelTest
	feedTrackers := !failed && !channelTest

	// ccmax evidence: a visible injection labels this request; the channel
	// becomes sticky only after repeated evidence (see ccmaxObserve)
	anthropicShape := strings.HasPrefix(relayInfo.UpstreamResponseId, "msg_")
	injected := anthropicShape && injectionDetected(relayInfo.GetEstimatePromptTokens(), injectedTokens(relayInfo))
	ccmaxHint := ccmaxObserve(channelId, injected, feedTrackers)
	src := InferUpstreamSource(channelType, relayInfo.UpstreamResponseHeaders, relayInfo.UpstreamResponseId, respModel, ccmaxHint)
	// backendShape is the wire-shape-only view (what the flip tracker watches);
	// backend is what gets displayed: when the shape is neutral and the source
	// carries official-vendor evidence, the displayed backend follows the source
	backendShape := inferBackendShape(channelType, relayInfo.UpstreamResponseId, respModel, ccmaxHint)
	backend := reconcileBackendWithSource(backendShape, src)

	ursp["sent"] = sentModel
	ursp["mismatch"] = mismatch
	ursp["kind"] = verdict // why it matched/mismatched (exact / snapshot_date / pinned_to_alias / mismatch / ...)
	ursp["src"] = src
	if audit.SentField != "" {
		ursp["sent_field"] = audit.SentField // UpstreamModelName when the wire carried a different model
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
	if len(audit.Models) > 1 {
		ursp["models"] = audit.Models // every distinct declaration, in order
	}
	if midStreamChange {
		ursp["mid_stream_change"] = true
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
	if relayInfo.ChannelMeta != nil && relayInfo.IsModelMapped {
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
	retryIndex := 0
	if ctx != nil {
		retryIndex = max(0, len(ctx.GetStringSlice("use_channel"))-1)
	}
	if retryIndex > 0 {
		ursp["retry"] = retryIndex
	}

	key := auditTrackerKey(relayInfo, sentModel)
	var from, bfrom string
	var flipped, bflipped bool
	backendSig := backendSignature(backendShape, respPrefix)
	if feedTrackers {
		from, flipped = trackSourceFlip(key, src)
		if flipped {
			ursp["src_flip"] = from + "->" + src
		}
		bfrom, bflipped = trackBackendFlip(key, backendSig)
		if bflipped {
			// both sides are raw shapes: the flip string describes the wire
			// shape change only; the reconciled label lives in ursp.backend
			ursp["backend_flip"] = bfrom + "->" + backendSig
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
		flipNote += "backend: " + bfrom + " -> " + backendSig
	}
	flipped = flipped || bflipped

	requestId := ""
	if ctx != nil {
		requestId = ctx.GetString(common.RequestIdKey)
	}
	signals := evaluateReplaySignals(relayInfo, channelId, requestId, !failed, feedTrackers)
	signals.apply(ursp)
	// root-only projection: LogOther strips root_info for admin and user
	// readers alike (user decision 2026-09-19: upstream-declared model data
	// is for the super administrator only)
	other.SetRoot("ursp", ursp)

	kind := "normal"
	var notes []string
	switch {
	case mismatch:
		kind = "model_mismatch"
	case midStreamChange:
		kind = "mid_stream_change"
	case signals.replay:
		kind = "replay_suspect"
	case flipped:
		kind = "provider_switch"
	}
	if kind == "provider_switch" {
		// exactly the flip, as before: the watchdog groups and renders these
		// notes as the source change text
		notes = append(notes, flipNote)
	} else {
		// the trigger reasons come first so the 255-char cut never drops
		// them; the watchdog's notification text is built from the note
		if reasons := signals.reasons(); len(reasons) > 0 {
			notes = append(notes, "signals: "+strings.Join(reasons, ","))
		}
		if signals.replay && kind != "replay_suspect" {
			notes = append(notes, "replay_suspect")
		}
		if verdict == VerdictPinnedToAlias {
			notes = append(notes, VerdictPinnedToAlias)
		}
		if failed {
			notes = append(notes, "error: "+errorType)
		}
		if channelTest {
			notes = append(notes, "channel_test")
		}
		if flipNote != "" {
			notes = append(notes, "provider also flipped: "+flipNote)
		}
		if midStreamChange {
			notes = append(notes, "mid_stream_change: "+strings.Join(audit.Models, " -> "))
		}
	}

	ev := &model.UpstreamEvidence{
		CreatedAt:         common.GetTimestamp(),
		Kind:              kind,
		Note:              strings.Join(notes, "; "),
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
		ModelFrame:        audit.ModelFrame,
		ErrorFrame:        audit.ErrorFrame,
		ExcerptTruncated:  audit.ExcerptTruncated,
		ExcerptFullLen:    audit.ExcerptFullLen,
		RetryIndex:        retryIndex,
		PromptSha256:      audit.PromptSHA256,
		OutputSha256:      audit.OutputSHA256,
	}
	if kind == "normal" && audit_setting.NormalExcerptMode() == audit_setting.NormalExcerptModeDigest {
		digestNormalEvidence(ev)
	}
	// built before the hand-off: once queued, the row belongs to the writer
	// goroutine, which sanitizes it in place
	logLine := "[upstream-audit] " + kind + " queued: sent=" + ev.SentModel +
		" got=" + ev.ResponseModel + " src=" + src + " " + ev.Note
	if enqueueUpstreamEvidence(ev) && kind != "normal" { // normal rows are high-volume — no per-row logging
		common.SysLog(logLine)
	}
}

// enqueueUpstreamEvidence hands evidence rows to the background writer;
// tests replace it to inspect the rows.
var enqueueUpstreamEvidence = model.EnqueueUpstreamEvidence

// evidenceDigestEdge is how much of each end a digest-mode normal row keeps.
const evidenceDigestEdge = 2 * 1024

// digestNormalEvidence reduces a normal row to its hashes plus the first and
// last 2KB of the excerpt and model frame (audit_setting digest mode).
// Anomaly rows never pass through here: they always keep the full excerpt.
func digestNormalEvidence(ev *model.UpstreamEvidence) {
	digest := func(s string) (string, bool) {
		if len(s) <= 2*evidenceDigestEdge {
			return s, false
		}
		head, _ := common.TruncateAuditBytes(s, evidenceDigestEdge)
		tail := common.TruncateAuditBytesTail(s, evidenceDigestEdge)
		return head + "\n…[digest: " + strconv.Itoa(len(s)-len(head)-len(tail)) + " bytes omitted]…\n" + tail, true
	}
	var cut bool
	ev.ResponseExcerpt, cut = digest(ev.ResponseExcerpt)
	ev.ExcerptTruncated = ev.ExcerptTruncated || cut
	ev.ModelFrame, _ = digest(ev.ModelFrame)
}
