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
	// replayMinOutputChars ignores outputs too short to be distinctive when
	// looking for duplicate outputs: agent openers and short JSON answers
	// repeat honestly across different prompts and at temperature > 0.
	replayMinOutputChars = 200
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
	promptDiffers    bool // recorded reason only: never enough on its own
	sampled          bool // recorded reason only: never enough on its own
	replay           bool
	implausibleSpeed bool
	extraKeys        []string
	ptRatio          float64
}

// reasons lists the replay-related signals that fired, for the evidence note.
func (s replaySignals) reasons() []string {
	var reasons []string
	for _, r := range []struct {
		on   bool
		name string
	}{
		{s.dupId, "dup_id"}, {s.dupOutput, "dup_output"}, {s.staleCreated, "stale_created"},
		{s.promptDiffers, "prompt_differs"}, {s.sampled, "sampled"}, {s.implausibleSpeed, "implausible_speed"},
	} {
		if r.on {
			reasons = append(reasons, r.name)
		}
	}
	return reasons
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

	created := relayInfo.UpstreamResponseCreated
	if created > 1e12 {
		created /= 1000
	}
	current := replayEntry{requestId: requestId, promptHash: audit.PromptSHA256, outputHash: audit.OutputSHA256, created: created, stale: s.staleCreated}
	// H2: the same response id on this channel for a different request
	var idPrev, outputPrev replayEntry
	if id := strings.TrimSpace(relayInfo.UpstreamResponseId); channelId != 0 && !isPlaceholderResponseId(id) {
		s.dupId, idPrev = replayMemory.seen("i|"+strconv.Itoa(channelId)+"|"+id, current, record)
	}
	// H3: the same (normalised) output text on this channel for a different
	// request; short outputs are not distinctive
	if channelId != 0 && audit.OutputSHA256 != "" && audit.OutputChars >= replayMinOutputChars {
		s.dupOutput, outputPrev = replayMemory.seen("o|"+strconv.Itoa(channelId)+"|"+audit.OutputSHA256, current, record)
	}
	s.promptDiffers = s.dupOutput && outputPrev.promptHash != "" && current.promptHash != "" && outputPrev.promptHash != current.promptHash
	s.sampled = s.dupOutput && audit.Temperature != nil && *audit.Temperature > 0
	// H4: a replay needs two independent facts pointing at the same earlier
	// answer. A duplicate id counts only when both sides produced the same,
	// non-empty output (tool-call-only turns and handlers without an output
	// hash never qualify: relays with fixed ids would otherwise flag every
	// such turn). Any non-empty text counts here — a cache replaying a short
	// greeting under the same id is the #148 case, while a constant-id
	// generator produces different text. Only the cross-id duplicate-output
	// rule needs a distinctive (≥200 char) output, and it counts only with a
	// corroborating stale created stamp. A different prompt or an explicit
	// temperature are recorded as reasons, not proof.
	// a stale created stamp only corroborates on channels whose created is not
	// a constant (#109 stamps every answer with the same old value). The
	// tracker is fed stale answers only (the && short-circuits): fresh answers
	// on a busy channel share one-second stamps all the time and would mark
	// any channel constant-created.
	staleCorroborates := s.staleCreated && !constantCreated.observe(channelId, created, current.outputHash, relayInfo.UpstreamResponseId, record)
	// same id + same text: deterministic answers behind a constant-id
	// upstream repeat honestly, so this needs one more fact —
	//   - a stale created that corroborates on this channel, or
	//   - the very same STALE created as the remembered answer (a cache hands
	//     back its old stamp; this holds even on a constant-created channel,
	//     while two fresh answers can share a one-second stamp), or
	//   - a distinctive (≥200 char) text for a different prompt (the same
	//     prompt answered identically is an honest repeated request)
	samePrompt := current.promptHash != "" && idPrev.promptHash != "" && current.promptHash == idPrev.promptHash
	sameStaleCreated := s.staleCreated && created != 0 && idPrev.created == created
	sameIdSameOutput := s.dupId && current.outputHash != "" && idPrev.outputHash == current.outputHash &&
		(staleCorroborates || sameStaleCreated || (audit.OutputChars >= replayMinOutputChars && !samePrompt))
	// a duplicate output with the same id on the same earlier row is the
	// same-id case above (including its same-prompt guard); across ids a
	// duplicate output needs a corroborating stale created. A cache hands back
	// an answer that was fresh when first produced; when the earlier answer
	// already carried this very stale stamp, the channel is stamping a
	// constant old created (#109) — possibly before the constant-created
	// tracker has seen two different answers (cold start) — so it is not proof.
	//
	// Only a fresh earlier answer counts: when the earlier copy was itself
	// already stale (whatever its stamp), both may just be answers from a
	// channel stamping an old constant created, including one that switched
	// constants in between (the tracker resets on a new stamp). A cache
	// replayed several times still counts every time, because a replayed copy
	// of a fresh answer is remembered as fresh (see replayLRU.seen).
	s.replay = sameIdSameOutput || (s.dupOutput && staleCorroborates && !outputPrev.stale)

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
	outputHash string
	created    int64
	stale      bool // created was already stale when this answer arrived
}

func newReplayLRU(limit int) *replayLRU {
	return &replayLRU{limit: limit, order: list.New(), entries: make(map[string]*list.Element)}
}

var replayMemory = newReplayLRU(replayCacheEntries)

// seen reports whether key was last recorded by a different request (a retry
// of the same request repeating an id is not a duplicate) and returns what
// that request recorded. With record set, the entry is (re)written for the
// current request.
func (l *replayLRU) seen(key string, current replayEntry, record bool) (dup bool, prev replayEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	current.key = key
	if el, ok := l.entries[key]; ok {
		entry := el.Value.(*replayEntry)
		dup = current.requestId == "" || entry.requestId != current.requestId
		prev = *entry
		if record {
			if current.outputHash == "" {
				// an output-less turn (tool calls only, or a handler that
				// computes no hash) must not erase the output to compare with
				current.outputHash, current.created, current.stale = entry.outputHash, entry.created, entry.stale
			}
			if !entry.stale && entry.created == current.created {
				// a replay of a fresh answer carries that answer's stamp: keep
				// the entry marked fresh, so the next replay of it still counts
				current.stale = false
			}
			*entry = current
			l.order.MoveToFront(el)
		}
		return dup, prev
	}
	if record {
		l.entries[key] = l.order.PushFront(&current)
		for l.order.Len() > l.limit {
			oldest := l.order.Back()
			l.order.Remove(oldest)
			delete(l.entries, oldest.Value.(*replayEntry).key)
		}
	}
	return false, replayEntry{}
}

// constantCreatedTracker remembers, per channel, the last STALE created stamp
// with the id and output it came with (fresh answers are never fed: they share
// one-second stamps honestly). The same stale stamp on a different id with a
// different output marks the channel as stamping a constant created, which
// makes stale_created meaningless there. One small entry per channel.
type constantCreatedTracker struct {
	channels sync.Map // channelId(int) -> *constantCreatedState
}

type constantCreatedState struct {
	mu       sync.Mutex
	created  int64
	id       string
	output   string
	constant bool
}

var constantCreated = &constantCreatedTracker{}

// observe feeds one answer (unless record is false) and reports whether the
// channel is known to stamp a constant created. Callers only feed answers
// whose created is already stale: fresh answers on a busy channel share a
// one-second stamp all the time, and feeding them would mark the channel as
// constant-created and silence stale_created as corroboration there.
func (c *constantCreatedTracker) observe(channelId int, created int64, output, id string, record bool) bool {
	if channelId == 0 || created == 0 {
		return false
	}
	v, _ := c.channels.LoadOrStore(channelId, &constantCreatedState{})
	st := v.(*constantCreatedState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if record {
		switch {
		case st.created != created:
			st.created, st.id, st.output, st.constant = created, id, output, false
		case id != "" && id != st.id && output != "" && st.output != "" && output != st.output:
			st.constant = true
		}
	}
	return st.constant
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
