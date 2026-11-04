package service

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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
	case strings.HasPrefix(alias, "gemini-") && len(suffix) == 3 && isAllDigits(suffix):
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
	if src := channelTypeBackend(channelType); src != "" {
		return src
	}
	if src := upstreamHeaderSource(channelType, headerKeysLower(headersJSON)); src != "" {
		return src
	}
	src, _ := inferBackendShape(channelType, strings.TrimSpace(respId), respModel, ccmaxHint)
	return src
}

// channelTypeBackend is the backend pinned by the channel protocol itself
// (AWS SigV4, Vertex, Azure deployments) — the strongest evidence there is.
func channelTypeBackend(channelType int) string {
	switch channelType {
	case constant.ChannelTypeAws:
		return "aws"
	case constant.ChannelTypeVertexAi:
		return "vertex"
	case constant.ChannelTypeAzure:
		return "azure"
	}
	return ""
}

// upstreamHeaderSource reads the hop from response headers: official vendor
// headers first (strongest response-side evidence), then relay software
// fingerprints. "" when the headers say nothing.
func upstreamHeaderSource(channelType int, h map[string]string) string {
	if h == nil {
		return ""
	}
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
	return ""
}

// officialSources are the header-derived sources that name the vendor that
// actually served the response (not a relay in between).
var officialSources = map[string]bool{
	"aws": true, "vertex": true, "azure": true, "google": true, "aistudio": true,
	"anthropic": true, "openai": true, "kimi": true,
}

// Backend provenance (ursp.backend_by) and confidence (ursp.backend_conf).
const (
	BackendByChannelType = "channel_type"
	BackendByHeader      = "header"
	BackendByBody        = "body"
	BackendByIdShape     = "id_shape"
	BackendByModelName   = "model_name"
	BackendByFallback    = "fallback"

	BackendConfStrong = "strong"
	BackendConfMedium = "medium"
	BackendConfWeak   = "weak"
	BackendConfNone   = "none"
)

// backendDecision is the backend behind the hop with where it came from.
// conflict lists every distinct vendor claim ("vendor:by") when the
// available evidence disagrees; the highest-priority claim wins.
type backendDecision struct {
	backend  string
	by       string
	conf     string
	conflict []string
}

// weakVendors lists the vendors named by weak evidence (model name, id shape)
// that lost to the decided backend on this reply.
func (d backendDecision) weakVendors() []string {
	var out []string
	for _, c := range d.conflict {
		vendor, by, _ := strings.Cut(c, ":")
		if vendor != d.backend && backendEvidenceStrength(by) == 1 {
			out = append(out, vendor)
		}
	}
	return out
}

// bodyBackends lists the backends named by vendor-specific body fields, in
// priority order (several at once only on a relay that mixes backends).
func bodyBackends(markers relaycommon.UpstreamBodyMarkers) []string {
	var out []string
	if markers.Has(relaycommon.BodyMarkerAzureContentFilter) {
		out = append(out, "azure")
	}
	if markers.Has(relaycommon.BodyMarkerBedrockMetrics) {
		out = append(out, "aws")
	}
	if markers.Has(relaycommon.BodyMarkerDeepSeekCacheHit | relaycommon.BodyMarkerDeepSeekCacheMiss) {
		out = append(out, "deepseek")
	}
	return out
}

// decideUpstreamBackend is the single place that decides the backend behind
// the hop, by evidence priority: channel protocol type (strong) > official
// vendor headers (medium) > vendor-specific body fields (medium; DeepSeek
// usage shape weak) > positive
// id / model shape (weak) > neutral protocol-format fallback (none).
func decideUpstreamBackend(channelType int, headerSource string, markers relaycommon.UpstreamBodyMarkers, id, model string, ccmaxHint bool) backendDecision {
	type claim struct{ vendor, by, conf string }
	var claims []claim
	if v := channelTypeBackend(channelType); v != "" {
		claims = append(claims, claim{v, BackendByChannelType, BackendConfStrong})
	}
	if officialSources[headerSource] {
		claims = append(claims, claim{headerSource, BackendByHeader, BackendConfMedium})
	}
	for _, v := range bodyBackends(markers) {
		conf := BackendConfMedium
		if v == "deepseek" {
			// The pure DeepSeek usage shape is documented, but nothing proves
			// only DeepSeek emits it, so it is displayed as weak. The flip
			// trackers still rank it by provenance (body, backendEvidenceStrength):
			// that strength governs hysteresis against intermittently missing
			// evidence, not how much the claim is believed.
			conf = BackendConfWeak
		}
		claims = append(claims, claim{v, BackendByBody, conf})
	}
	shape, shapeBy := inferBackendShape(channelType, strings.TrimSpace(id), model, ccmaxHint)
	if shapeBy != BackendByFallback {
		claims = append(claims, claim{shape, shapeBy, BackendConfWeak})
	}
	if len(claims) == 0 {
		return backendDecision{backend: shape, by: BackendByFallback, conf: BackendConfNone}
	}
	d := backendDecision{backend: claims[0].vendor, by: claims[0].by, conf: claims[0].conf}
	for _, c := range claims[1:] {
		if srcFamily(c.vendor) != srcFamily(d.backend) {
			for _, all := range claims {
				d.conflict = append(d.conflict, all.vendor+":"+all.by)
			}
			break
		}
	}
	return d
}

// inferBackendShape classifies a response by wire shape alone (id prefix +
// model naming), ignoring headers and channel type, and says which of the two
// decided it (id_shape / model_name), or fallback when only the protocol
// format is known (gpt_relay / claude_relay / unknown).
func inferBackendShape(channelType int, id, respModel string, ccmaxHint bool) (backend, by string) {
	modelLower := strings.ToLower(respModel)
	switch {
	case strings.HasPrefix(id, "msg_bdrk_"):
		return "aws", BackendByIdShape
	case isBedrockModelId(respModel):
		return "aws", BackendByModelName
	case strings.HasPrefix(id, "msg_vrtx_"), strings.HasPrefix(id, "req_vrtx_"):
		return "vertex", BackendByIdShape
	case strings.HasPrefix(id, "msg_"):
		if ccmaxHint {
			return "ccmax", BackendByIdShape // anthropic wire shape + injected-prompt evidence = Max subscription relay
		}
		return "claude_relay", BackendByFallback // anthropic wire shape without vendor evidence
	case strings.HasPrefix(id, "gen-"):
		return "openrouter", BackendByIdShape
	case strings.HasPrefix(id, "chatcmpl-"), strings.HasPrefix(id, "resp_"):
		// OpenAI chat / Responses API wire shape without vendor evidence
		switch {
		case strings.Contains(modelLower, "deepseek"):
			return "deepseek", BackendByModelName
		case strings.Contains(modelLower, "glm"):
			return "glm", BackendByModelName
		case strings.Contains(modelLower, "kimi") || strings.Contains(modelLower, "moonshot"):
			return "kimi", BackendByModelName
		default:
			return "gpt_relay", BackendByFallback
		}
	case uuidIdRe.MatchString(id):
		if strings.Contains(modelLower, "deepseek") {
			return "deepseek", BackendByModelName
		}
		return "unknown", BackendByFallback
	case numericIdRe.MatchString(id):
		return "glm", BackendByIdShape // Zhipu-style timestamp ids
	case channelType == constant.ChannelTypeGemini && id != "":
		return "aistudio", BackendByModelName // Gemini channel answering with a modelVersion/responseId
	}
	return "unknown", BackendByFallback
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
	// path partitions the source tracker only (see its call site); the
	// backend tracker leaves it empty.
	path string
}

type channelSrcState struct {
	mu   sync.Mutex
	last string
	keys []string // source-naming header keys of the last answer (see sourceHeaderKeys)
}

var srcTracker sync.Map     // flipTrackerKey -> *channelSrcState
var backendTracker sync.Map // flipTrackerKey -> *backendTrackState (decided backend + vendor prefix segment)

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

// backendEvidenceLostAfter is how many consecutive responses without any
// vendor evidence it takes before a backend identified by headers or body
// fingerprints is reported as lost. Single misses are normal (an Azure
// deployment with partial annotations, a relay stripping headers on some
// paths) and must not create a provider_switch row each way.
const backendEvidenceLostAfter = 20

// backendTrackState is the per-key state of the backend tracker.
type backendTrackState struct {
	mu      sync.Mutex
	last    string // last backend signature the key settled on
	lastBy  string // evidence behind last (BackendBy*)
	missing int    // consecutive answers without vendor evidence since then
	lost    bool   // the evidence-lost event was recorded
	// companions are the vendors named by weak evidence (model name, id
	// shape) on the same replies as the header/body/channel-type evidence
	// behind last: Azure body fields prove the platform while the model
	// name names the model (Azure-hosted DeepSeek-R1), so a later reply
	// carrying only such a weak claim is "no evidence", not a switch.
	companions []string
}

// maxBackendCompanions bounds backendTrackState.companions.
const maxBackendCompanions = 4

// backendEvidenceStrength orders provenance: a same-vendor answer never
// downgrades the evidence a key's backend is known from.
func backendEvidenceStrength(by string) int {
	switch by {
	case BackendByChannelType:
		return 3
	case BackendByHeader, BackendByBody:
		return 2
	case BackendByIdShape, BackendByModelName:
		return 1
	}
	return 0
}

// backendTrackEvent describes a recorded backend change: flip is the
// "from->to" value for ursp.backend_flip, note the evidence-note text.
type backendTrackEvent struct {
	flip string
	note string
}

// trackBackendFlip watches the decided backend behind a channel (see
// decideUpstreamBackend) independently of the hop label, so a relay that
// silently moves from Bedrock to a Claude relay, or to another vendor, is
// caught even though its own headers never change. The signature includes
// the vendor prefix the response carried (gpt_relay/deepseek).
//
//   - Two positive decisions that disagree are a flip, immediately. Only the
//     wire shape, or the vendor between two responses that both carry one,
//     counts: gpt-4o / openai/gpt-4o or Bedrock region prefixes are the same.
//   - Every first decision seeds the key, a fallback included, so a key that
//     starts without evidence still sees its first real switch (gpt_relay →
//     openrouter).
//   - A fallback after a backend known from its id / model shape, or after
//     another fallback, is compared like any other answer: ids and model
//     names are on every answer, so their change is real (msg_bdrk_ → msg_).
//   - After a backend known from the channel type, headers or body
//     fingerprints, an answer without vendor evidence is "no evidence this
//     time", not a switch: the last backend is kept. That covers a fallback
//     (by == fallback, whatever vendor prefix it carries) and an answer whose
//     only evidence is a weak claim for a vendor seen next to that evidence
//     before (its companions: Azure body fields prove the platform, the
//     DeepSeek model name names the model). Only backendEvidenceLostAfter
//     consecutive such answers record one "evidence lost" event.
//   - "Lost" is cleared only by channel-type/header/body evidence: for the
//     same vendor it records "evidence restored", for another vendor a switch
//     "backend: a -> b (after evidence lost)". Weak evidence for the same
//     vendor stays under the lost state; weak evidence for another vendor is
//     a normal switch.
//   - A same-vendor answer never downgrades the provenance the backend is
//     known from (aws by body, then aws by id shape, stays body-backed).
//   - A backend known only from weak evidence (deepseek by model name) that
//     is followed by header/body/channel-type evidence for another vendor is
//     a switch, even when that evidence names the weak vendor as a companion:
//     "model name unchanged, backend swapped" is exactly what this tracker
//     is for. After a restart this records at most one row per key when the
//     first reply happened to carry only weak evidence (accepted trade-off).
//   - The vendor prefix only takes part in comparing two answers' vendor
//     segments (present ↔ absent is the same backend).
func trackBackendFlip(key flipTrackerKey, backend, by string, weakVendors []string) backendTrackEvent {
	if key.channelId == 0 || backend == "" || backend == "unknown" {
		return backendTrackEvent{}
	}
	v, _ := backendTracker.LoadOrStore(key, &backendTrackState{})
	st := v.(*backendTrackState)
	st.mu.Lock()
	defer st.mu.Unlock()
	prev := st.last
	evidence := isEvidenceBackedBy(by)
	if prev == "" {
		// seed from any decision, a fallback included: a key that starts
		// without vendor evidence must still see its first real switch
		st.last, st.lastBy = backend, by
		if evidence {
			st.companions = boundedCompanions(nil, weakVendors)
		}
		return backendTrackEvent{}
	}
	shape, vendor, _ := strings.Cut(backend, "/")
	// "no evidence": a fallback (whatever vendor prefix it carries), or only
	// a weak claim for a vendor that was seen next to the evidence before
	noEvidence := by == BackendByFallback ||
		(backendEvidenceStrength(by) == 1 && slices.Contains(st.companions, shape))
	if noEvidence && isEvidenceBackedBy(st.lastBy) {
		if st.lost {
			return backendTrackEvent{}
		}
		st.missing++
		if st.missing < backendEvidenceLostAfter {
			return backendTrackEvent{}
		}
		st.lost = true
		return backendTrackEvent{
			flip: prev + "->(none)",
			note: "backend evidence lost: " + prev + " -> (none) after " + strconv.Itoa(st.missing),
		}
	}
	prevShape, prevVendor, _ := strings.Cut(prev, "/")
	sameBackend := srcFamily(prevShape) == srcFamily(shape) && (vendor == "" || prevVendor == "" || vendor == prevVendor)
	if st.lost {
		if evidence {
			st.lost, st.missing = false, 0
			st.last, st.lastBy = backend, by
			st.companions = boundedCompanions(nil, weakVendors)
			if !sameBackend {
				// evidence came back for another vendor: that is a switch
				return backendTrackEvent{flip: prev + "->" + backend, note: "backend: " + prev + " -> " + backend + " (after evidence lost)"}
			}
			return backendTrackEvent{flip: "(none)->" + backend, note: "backend evidence restored: (none) -> " + backend}
		}
		if sameBackend {
			return backendTrackEvent{} // weak evidence for the same vendor does not restore
		}
		st.lost = false // weak evidence for another vendor: a switch, recorded below
	}
	st.missing = 0
	if sameBackend {
		if vendor != "" || prevVendor == "" {
			st.last = backend
		}
		if backendEvidenceStrength(by) >= backendEvidenceStrength(st.lastBy) {
			st.lastBy = by
		}
		if evidence {
			st.companions = boundedCompanions(st.companions, weakVendors)
		}
		return backendTrackEvent{}
	}
	st.last, st.lastBy = backend, by
	st.companions = nil
	if evidence {
		st.companions = boundedCompanions(nil, weakVendors)
	}
	return backendTrackEvent{flip: prev + "->" + backend, note: "backend: " + prev + " -> " + backend}
}

// boundedCompanions merges vendors into companions without duplicates, up to
// maxBackendCompanions.
func boundedCompanions(companions, vendors []string) []string {
	for _, v := range vendors {
		if len(companions) >= maxBackendCompanions {
			break
		}
		if v != "" && !slices.Contains(companions, v) {
			companions = append(companions, v)
		}
	}
	return companions
}

// isEvidenceBackedBy reports whether a backend decision came from evidence
// that can be intermittently absent (headers stripped on some paths,
// annotations off or partial), so its absence gets hysteresis instead of an
// immediate flip.
func isEvidenceBackedBy(by string) bool {
	return by == BackendByChannelType || by == BackendByHeader || by == BackendByBody
}

// trackSourceFlip watches the hop label (src) of a tracker key; a change
// between two known labels of different families is a flip. headerKeys are
// the answer's source-naming header keys; on a flip the previous answer's are
// returned so the evidence can say which headers changed.
func trackSourceFlip(key flipTrackerKey, src string, headerKeys []string) (from string, prevKeys []string, flipped bool) {
	if key.channelId == 0 || src == "" || src == "unknown" {
		return "", nil, false
	}
	v, _ := srcTracker.LoadOrStore(key, &channelSrcState{})
	st := v.(*channelSrcState)
	st.mu.Lock()
	defer st.mu.Unlock()
	prev, prevKeys := st.last, st.keys
	st.last, st.keys = src, headerKeys
	if prev == "" || srcFamily(prev) == srcFamily(src) {
		return "", nil, false
	}
	return prev, prevKeys, true
}

// sourceHeaderKeys lists, sorted, the response header names that on their own
// name a source (a vendor's or a relay software's header).
func sourceHeaderKeys(channelType int, h map[string]string) []string {
	var keys []string
	for k := range h {
		if upstreamHeaderSource(channelType, map[string]string{k: ""}) != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// sourceHeaderChange renders which source-naming headers disappeared (-) and
// appeared (+) between two answers, at most four of each: the first thing to
// read when judging whether a source flip is a header-forwarding change or a
// different backend.
func sourceHeaderChange(prev, cur []string) string {
	const maxEach = 4
	var gone, came []string
	for _, k := range prev {
		if !slices.Contains(cur, k) && len(gone) < maxEach {
			gone = append(gone, "-"+k)
		}
	}
	for _, k := range cur {
		if !slices.Contains(prev, k) && len(came) < maxEach {
			came = append(came, "+"+k)
		}
	}
	return strings.Join(append(gone, came...), " ")
}

// auditNoteSegment makes one segment of an evidence note safe: upstream text
// (model names, error codes, header names, vendor prefixes) must not add
// segments (';' '|') or lines, or disguise the alert text the watchdog builds
// from the note (Cf: bidi overrides, zero-width characters; Zl/Zp: U+2028/9).
func auditNoteSegment(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ';' || r == '|' || unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return '_'
		}
		return r
	}, s)
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
	// The prefix is upstream text. A vendor is written in ASCII only: no
	// whitespace or arrows the watchdog would read as "a -> b", nothing
	// invisible or look-alike. Any other character is spelled as its code point
	// (u5b98), so two different vendors never collapse into one. Bounded, so
	// tracker state and switch notes stay bounded.
	const maxVendor = 32
	var vb strings.Builder
	for _, r := range vendor {
		var tok string
		switch {
		case r < 0x80 && (r == '_' || r == '.' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r)):
			tok = string(r)
		case unicode.IsSpace(r), r == ';', r == '|', r == '<', r == '>':
			tok = "_"
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue // invisible: dropped, so what reads the same compares the same
		default:
			tok = "u" + strconv.FormatInt(int64(r), 16)
		}
		if vb.Len()+len(tok) > maxVendor {
			break // whole characters only
		}
		vb.WriteString(tok)
	}
	vendor = vb.String()
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
	// A stream that ended in an upstream failure (Responses response.failed)
	// is not an error attempt here, yet its frames describe the failure, not
	// the backend, and its placeholder ids (resp_pv_503) repeat across
	// requests: it feeds neither the switch trackers nor the replay memory.
	streamFailed := !failed && relayInfo.StreamStatus.ResponseFailed()
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
	// an empty upstream stream carries no body to judge, but its headers (the
	// upstream's request id) are what tracing it with the upstream needs
	emptyStream := failed && relayInfo.StreamStatus.UpstreamSentNothing()
	// a stream that failed with only an error event still needs its evidence
	// row: the stream_failed note is that failure's only alert path
	if respModel == "" && relayInfo.UpstreamResponseId == "" && relayInfo.UpstreamResponseExcerpt == "" && !emptyStream && !streamFailed {
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
	feedTrackers := !failed && !streamFailed && !channelTest

	// ccmax evidence: a visible injection labels this request; the channel
	// becomes sticky only after repeated evidence (see ccmaxObserve)
	anthropicShape := strings.HasPrefix(relayInfo.UpstreamResponseId, "msg_")
	injected := anthropicShape && injectionDetected(relayInfo.GetEstimatePromptTokens(), injectedTokens(relayInfo))
	ccmaxHint := ccmaxObserve(channelId, injected, feedTrackers)
	// body fingerprints of a failed attempt describe an error body, never the
	// backend that would have answered
	markers := audit.BodyMarkers
	if failed {
		markers = 0
	}
	responseHeaders := headerKeysLower(relayInfo.UpstreamResponseHeaders)
	headerSource := upstreamHeaderSource(channelType, responseHeaders)
	// src is the direct hop: channel type, then headers; without either, a
	// NewAPI usage field still identifies a NewAPI hop, else the wire shape
	src := channelTypeBackend(channelType)
	switch {
	case src != "":
	case headerSource != "":
		src = headerSource
	case markers.Has(relaycommon.BodyMarkerNewAPICacheSplit):
		src = "newapi_relay"
	default:
		src, _ = inferBackendShape(channelType, strings.TrimSpace(relayInfo.UpstreamResponseId), respModel, ccmaxHint)
	}
	decision := decideUpstreamBackend(channelType, headerSource, markers, relayInfo.UpstreamResponseId, respModel, ccmaxHint)
	backend := decision.backend

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
	ursp["backend_by"] = decision.by     // channel_type / header / body / id_shape / model_name / fallback
	ursp["backend_conf"] = decision.conf // strong / medium / weak / none
	if len(decision.conflict) > 0 {
		ursp["backend_conflict"] = decision.conflict // e.g. ["openai:header", "azure:body"]
	}
	if backend != "unknown" {
		ursp["backend"] = backend // backend behind the hop (aws / azure / vertex / gpt_relay fallback / ...)
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
	var from, headerChange string
	var flipped bool
	var backendEvent backendTrackEvent
	// the tracker follows the decided backend, with hysteresis for answers
	// that carry no vendor evidence (see trackBackendFlip)
	backendSig := backendSignature(backend, respPrefix)
	if feedTrackers {
		// The source comes from response headers, and a relay can pass the
		// vendor's headers through on one path and strip them on another
		// (non-stream with x-goog-*, stream without). Each path is compared
		// only with its own history, so a change on a path is still reported
		// once while the difference between paths never is. The stream flag is
		// the client's: handlers widen IsStream when an upstream answers with
		// a stream, and an upstream that starts doing so is itself a change
		// to report against the request's own history.
		clientStream := relayInfo.IsStream
		if ctx != nil {
			if v, ok := ctx.Get(string(constant.ContextKeyIsStream)); ok {
				clientStream, _ = v.(bool)
			}
		}
		srcKey := key
		srcKey.path = string(relayInfo.GetFinalRequestRelayFormat()) + "|" + strconv.FormatBool(clientStream)
		if relayInfo.UpstreamAudit.Handler == "responses_ws" {
			// a websocket turn is answered over another transport than SSE
			srcKey.path += "|ws"
		}
		// headerSource is empty exactly when no header names a source, and a
		// channel-type source never flips: only then is there anything to list
		var curKeys []string
		if headerSource != "" && channelTypeBackend(channelType) == "" {
			curKeys = sourceHeaderKeys(channelType, responseHeaders)
		}
		var prevKeys []string
		from, prevKeys, flipped = trackSourceFlip(srcKey, src, curKeys)
		if flipped {
			headerChange = sourceHeaderChange(prevKeys, curKeys)
			ursp["src_flip"] = from + "->" + src
		}
		backendEvent = trackBackendFlip(key, backendSig, decision.by, decision.weakVendors())
		if backendEvent.flip != "" {
			ursp["backend_flip"] = backendEvent.flip
		}
	}
	// one evidence row per exchange: a source flip and a backend event on the
	// same answer share it, as segments of one note. The note is cut at 255
	// characters when stored, so the bounded change segments come first and the
	// header list, the only open-ended part, comes last and is capped itself.
	var flipSegs []string
	if flipped {
		flipSegs = append(flipSegs, from+" -> "+src)
	}
	if backendEvent.note != "" {
		flipSegs = append(flipSegs, backendEvent.note)
	}
	headersSeg := ""
	if flipped && headerChange != "" {
		headersSeg, _ = common.TruncateAuditBytes("headers: "+headerChange, 96)
	}
	flipped = flipped || backendEvent.flip != ""

	requestId := ""
	if ctx != nil {
		requestId = ctx.GetString(common.RequestIdKey)
	}
	signals := evaluateReplaySignals(relayInfo, channelId, requestId, !failed && !streamFailed, feedTrackers)
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
		// exactly the flip: the watchdog groups on the "a -> b" segments and
		// renders the headers segment separately
		notes = append(notes, flipSegs...)
		if headersSeg != "" {
			notes = append(notes, headersSeg)
		}
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
		if streamFailed {
			// the watchdog alerts on these: the log row itself reads as a
			// zero-billed success. The upstream-controlled code stays short.
			outcome := relayInfo.StreamStatus.OutcomeSnapshot()
			// it reaches a phone alert: an identifier, nothing that reads as prose
			code := strings.Map(func(r rune) rune {
				if r < 0x80 && (r == '_' || r == '.' || r == ':' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
					return r
				}
				return '_'
			}, cmp.Or(outcome.ErrorCode, outcome.ErrorType, "unknown"))
			code, _ = common.TruncateAuditBytes(code, 64)
			notes = append(notes, "stream_failed: "+code)
		}
		if emptyStream {
			skipped, _ := relayInfo.StreamStatus.SkippedLines()
			notes = append(notes, "empty_stream: end_reason="+string(relayInfo.StreamStatus.EndReason)+" non_data_lines="+strconv.Itoa(skipped))
		}
		if channelTest {
			notes = append(notes, "channel_test")
		}
		if len(flipSegs) > 0 {
			notes = append(notes, "provider also flipped: "+strings.Join(flipSegs, ", "))
		}
		if midStreamChange {
			notes = append(notes, "mid_stream_change: "+strings.Join(audit.Models, " -> "))
		}
	}

	// The stored note is cut at 255 characters. Segments are kept whole, in
	// order, while they fit: a cut inside one would leave a fragment the
	// watchdog reads as a segment of its own. Only a lone first segment, or a
	// trailing model sequence (cut after its last whole model), is shortened.
	const noteBudget = 255
	kept, used := notes[:0], 0
	for _, seg := range notes {
		seg = auditNoteSegment(seg)
		cost := utf8.RuneCountInString(seg)
		if len(kept) > 0 {
			cost += 2 // "; "
		}
		if used+cost > noteBudget {
			// a lone first segment, or a trailing model sequence, is cut instead of
			// dropped: either still starts with its own prefix
			const midStreamPrefix = "mid_stream_change: "
			if len(kept) == 0 {
				kept = append(kept, common.TruncateAuditRunes(seg, noteBudget))
			} else if room := noteBudget - used - 2; strings.HasPrefix(seg, midStreamPrefix) && room > len(midStreamPrefix) {
				// never end on half a model name: cut after the last whole one, or
				// leave the segment out when not even the first one fits
				cut := common.TruncateAuditRunes(seg, room)
				if i := strings.LastIndex(cut, " -> "); i > len(midStreamPrefix) {
					kept = append(kept, cut[:i])
				}
			}
			break
		}
		kept, used = append(kept, seg), used+cost
	}
	notes = kept
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
	logLine := "[upstream-audit] " + kind + " queued: sent=" + auditNoteSegment(ev.SentModel) +
		" got=" + auditNoteSegment(ev.ResponseModel) + " src=" + src + " " + ev.Note
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
	ev.ErrorFrame, _ = digest(ev.ErrorFrame)
}
