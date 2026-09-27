package service

import (
	"container/list"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/tidwall/gjson"
)

// ── replay / authenticity signals (spec H) ──────────────────────────────────
// Weak, root-only screening signals recorded in ursp. Only the replay verdict
// (a duplicate response id, or a duplicate output backed by a second
// independent reason) changes the evidence kind; every other flag is a label.

const (
	// staleCreatedSlack tolerates clock skew and queueing between the
	// request start and the upstream stamping its response.
	staleCreatedSlack = 120
	// replayMinOutputChars ignores outputs too short to be distinctive
	// ("OK", "Hello!") when looking for duplicate outputs.
	replayMinOutputChars = 64
	// replayCacheEntries bounds the in-memory replay memory across channels.
	replayCacheEntries = 100_000
	// implausibleSpeedMinTokens / implausibleSpeedTokensPerSec: a long output
	// generated faster than any real deployment streams is weak evidence of
	// a cached answer.
	implausibleSpeedMinTokens    = 200
	implausibleSpeedTokensPerSec = 400
)

type replaySignals struct {
	staleCreated     bool
	dupId            bool
	dupOutput        bool
	replay           bool
	implausibleSpeed bool
	extraKeys        []string
	ptRatio          float64
}

func (s replaySignals) apply(ursp map[string]any) {
	if s.staleCreated {
		ursp["stale_created"] = true
	}
	if s.dupId {
		ursp["dup_id"] = true
	}
	if s.dupOutput {
		ursp["dup_output"] = true
	}
	if s.replay {
		ursp["replay"] = true
	}
	if s.implausibleSpeed {
		ursp["implausible_speed"] = true
	}
	if len(s.extraKeys) > 0 {
		ursp["extra_keys"] = s.extraKeys
	}
	if s.ptRatio > 0 {
		ursp["pt_ratio"] = s.ptRatio
	}
}

// evaluateReplaySignals computes the H1–H7 signals for one exchange. success
// is false for failed attempts (only the created-time check applies to them);
// record is false for failed attempts and channel tests, which may look up
// the replay memory but never add to it.
func evaluateReplaySignals(relayInfo *relaycommon.RelayInfo, channelId int, requestId string, success, record bool) replaySignals {
	var s replaySignals
	audit := &relayInfo.UpstreamAudit
	now := auditNow()

	// H1: created outside [request start − slack, now + slack]; ms stamps
	// are normalised. Weak alone: some relays return a constant created.
	if created := relayInfo.UpstreamResponseCreated; created > 0 {
		if created > 1e12 {
			created /= 1000
		}
		start := relayInfo.StartTime.Unix()
		if relayInfo.StartTime.IsZero() {
			start = now.Unix()
		}
		s.staleCreated = created < start-staleCreatedSlack || created > now.Unix()+staleCreatedSlack
	}
	if !success {
		return s
	}

	// H2: the same response id on this channel for a different request
	var prevPrompt string
	if id := strings.TrimSpace(relayInfo.UpstreamResponseId); channelId != 0 && !isPlaceholderResponseId(id) {
		s.dupId, _ = replayMemory.seen("i|"+strconv.Itoa(channelId)+"|"+id, requestId, audit.PromptSHA256, record)
	}
	// H3: the same (normalised) output on this channel for a different request
	if channelId != 0 && audit.OutputSHA256 != "" && audit.OutputChars >= replayMinOutputChars {
		s.dupOutput, prevPrompt = replayMemory.seen("o|"+strconv.Itoa(channelId)+"|"+audit.OutputSHA256, requestId, audit.PromptSHA256, record)
	}
	// H4: a duplicate id is enough; a duplicate output needs a second reason
	// that rules out an honest deterministic repeat
	promptDiffers := s.dupOutput && prevPrompt != "" && audit.PromptSHA256 != "" && prevPrompt != audit.PromptSHA256
	sampled := audit.Temperature != nil && *audit.Temperature > 0
	s.replay = s.dupId || (s.dupOutput && (s.staleCreated || promptDiffers || sampled))

	// H5: generation speed. Streams are timed from the first byte; a
	// non-stream response has no first-token time, so its whole duration
	// is used (an instant long answer is the case of interest).
	if completion := audit.CompletionTokens; completion >= implausibleSpeedMinTokens && !relayInfo.StartTime.IsZero() {
		seconds := now.Sub(relayInfo.StartTime).Seconds()
		if relayInfo.IsStream && relayInfo.FirstResponseTime.After(relayInfo.StartTime) {
			seconds -= relayInfo.FirstResponseTime.Sub(relayInfo.StartTime).Seconds()
		}
		s.implausibleSpeed = float64(completion)/math.Max(0.001, seconds) > implausibleSpeedTokensPerSec
	}

	// H6: top-level response keys outside the protocol's standard set
	s.extraKeys = unknownResponseKeys(audit.Handler, relayInfo)

	// H7: upstream-reported prompt tokens against our own estimate
	if est := relayInfo.GetEstimatePromptTokens(); est > 0 && audit.PromptTokens > 0 && !audit.TokensLocal {
		s.ptRatio = math.Round(float64(audit.PromptTokens)/float64(est)*100) / 100
	}
	return s
}

// isPlaceholderResponseId filters ids that cannot identify a response: empty
// or very short ids, and prefix-only or all-zero ids (resp_0, chatcmpl-0).
func isPlaceholderResponseId(id string) bool {
	if len(id) < 8 {
		return true
	}
	rest := id
	if i := strings.LastIndexAny(id, "-_"); i >= 0 {
		rest = id[i+1:]
	}
	return strings.Trim(rest, "0") == ""
}

// replayLRU remembers which request last produced a response id / output on
// a channel. Bounded, least-recently-seen entries are evicted first.
type replayLRU struct {
	mu      sync.Mutex
	limit   int
	order   *list.List
	entries map[string]*list.Element
}

type replayEntry struct {
	key        string
	requestId  string
	promptHash string
}

func newReplayLRU(limit int) *replayLRU {
	return &replayLRU{limit: limit, order: list.New(), entries: make(map[string]*list.Element)}
}

var replayMemory = newReplayLRU(replayCacheEntries)

// seen reports whether key was last recorded by a different request (a retry
// of the same request repeating an id is not a duplicate) and returns that
// request's prompt hash. With record set, the entry is (re)written for this
// request.
func (l *replayLRU) seen(key, requestId, promptHash string, record bool) (dup bool, prevPrompt string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.entries[key]; ok {
		entry := el.Value.(*replayEntry)
		dup = requestId == "" || entry.requestId != requestId
		prevPrompt = entry.promptHash
		if record {
			entry.requestId, entry.promptHash = requestId, promptHash
			l.order.MoveToFront(el)
		}
		return dup, prevPrompt
	}
	if record {
		l.entries[key] = l.order.PushFront(&replayEntry{key: key, requestId: requestId, promptHash: promptHash})
		for l.order.Len() > l.limit {
			oldest := l.order.Back()
			l.order.Remove(oldest)
			delete(l.entries, oldest.Value.(*replayEntry).key)
		}
	}
	return false, ""
}

// knownResponseKeys are the standard top-level keys of each protocol's
// response object (stream chunks included). Anything else is reported as
// ursp.extra_keys — e.g. the random "provider" / "api_key_id" fields a
// caching relay added to replayed answers.
var knownResponseKeys = map[string][]string{
	"openai_chat": {
		"id", "object", "created", "model", "choices", "usage", "system_fingerprint",
		"service_tier", "prompt_filter_results", "obfuscation",
	},
	"openai_responses": {
		"id", "object", "created_at", "status", "background", "billing", "conversation", "error",
		"incomplete_details", "instructions", "max_output_tokens", "max_tool_calls", "metadata",
		"model", "output", "output_text", "parallel_tool_calls", "previous_response_id", "prompt",
		"prompt_cache_key", "prompt_cache_retention", "reasoning", "safety_identifier", "service_tier",
		"store", "temperature", "text", "tool_choice", "tools", "top_logprobs", "top_p", "truncation",
		"usage", "user", "completed_at", "frequency_penalty", "presence_penalty",
	},
	"claude": {
		"id", "type", "role", "model", "content", "stop_reason", "stop_sequence", "usage",
		"container", "context_management",
	},
	"gemini": {
		"candidates", "usageMetadata", "modelVersion", "responseId", "promptFeedback", "createTime",
	},
}

// responseProtocolOf maps an observer handler to the protocol of the
// upstream's response.
func responseProtocolOf(handler string) string {
	switch {
	case strings.HasPrefix(handler, "openai_chat"), strings.HasPrefix(handler, "chat_to_responses"), strings.HasPrefix(handler, "xai_chat"):
		return "openai_chat"
	case strings.HasPrefix(handler, "openai_responses"), strings.HasPrefix(handler, "responses_to_chat"):
		return "openai_responses"
	case strings.HasPrefix(handler, "claude"), strings.HasPrefix(handler, "aws_claude"):
		return "claude"
	case strings.HasPrefix(handler, "gemini"):
		return "gemini"
	}
	return ""
}

func unknownResponseKeys(handler string, relayInfo *relaycommon.RelayInfo) []string {
	protocol := responseProtocolOf(handler)
	known := knownResponseKeys[protocol]
	if known == nil {
		return nil
	}
	frame := relayInfo.UpstreamAudit.ModelFrame
	if frame == "" || !relayInfo.IsStream {
		frame = relayInfo.UpstreamResponseExcerpt
	}
	object := gjson.Parse(frame)
	// stream events wrap the response object: Claude message_start carries
	// it under "message", Responses events under "response"
	if protocol == "claude" && object.Get("type").String() == "message_start" {
		object = object.Get("message")
	} else if protocol == "openai_responses" && object.Get("response").IsObject() {
		object = object.Get("response")
	}
	if !object.IsObject() {
		return nil
	}
	var extra []string
	object.ForEach(func(k, _ gjson.Result) bool {
		if !slices.Contains(known, k.String()) {
			extra = append(extra, k.String())
		}
		return len(extra) < 16
	})
	slices.Sort(extra)
	return extra
}
