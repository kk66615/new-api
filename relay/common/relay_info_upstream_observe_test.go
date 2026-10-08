package common

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A converted relay may emit a terminal event without a model; the id must
// still be captured so the exchange is auditable (model-less ursp).
func TestObserveUpstreamResponseKeepsIdWithoutModel(t *testing.T) {
	info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "gpt-5.6-sol"}}
	info.ObserveUpstreamResponse("", "resp_0", 1788619663, "")
	if info.UpstreamResponseModel != "" {
		t.Fatalf("model must stay empty, got %q", info.UpstreamResponseModel)
	}
	if info.UpstreamResponseId != "resp_0" || info.UpstreamResponseCreated != 1788619663 {
		t.Fatalf("id/created not captured: %q %d", info.UpstreamResponseId, info.UpstreamResponseCreated)
	}
	if info.UpstreamSentModel != "gpt-5.6-sol" {
		t.Fatalf("sent-model snapshot missing: %q", info.UpstreamSentModel)
	}
	// a later declaration with a model fills it in without erasing the id
	info.ObserveUpstreamResponse("gpt-5.6-sol", "", 0, "")
	if info.UpstreamResponseModel != "gpt-5.6-sol" || info.UpstreamResponseId != "resp_0" {
		t.Fatalf("second observation wrong: %q %q", info.UpstreamResponseModel, info.UpstreamResponseId)
	}
}

// Handlers are exercised by upstream tests with a bare RelayInfo (nil
// ChannelMeta); the forensics hooks must never dereference it.
func TestObserveUpstreamResponseNilChannelMeta(t *testing.T) {
	info := &RelayInfo{}
	info.ObserveUpstreamResponse("gpt-5.6-sol", "resp_0", 0, "")
	info.ObserveUpstreamUsage(1, 2, 3)
	info.SetUpstreamResponseForensics("{}", nil)
	if info.UpstreamResponseModel != "gpt-5.6-sol" || info.UpstreamResponseId != "resp_0" || info.UpstreamSentModel != "" {
		t.Fatalf("unexpected state: %q %q %q", info.UpstreamResponseModel, info.UpstreamResponseId, info.UpstreamSentModel)
	}
}

// The whole point of the sent-model snapshot is that it survives the claude
// stream handlers overwriting UpstreamModelName with the model the UPSTREAM
// declared. If the snapshot were taken after that rewrite, every comparison
// would be model-against-itself and the audit would become a silent no-op.
func TestSentModelSnapshotSurvivesUpstreamModelNameRewrite(t *testing.T) {
	info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "claude-sonnet-5"}}
	info.SnapshotUpstreamSentModel()

	// what the claude stream handler does on message_start
	info.ObserveUpstreamResponse("claude-sonnet-4-6", "msg_01abc", 0, "")
	info.UpstreamModelName = "claude-sonnet-4-6"

	if info.UpstreamSentModel != "claude-sonnet-5" {
		t.Fatalf("snapshot was corrupted by the rewrite: got %q, want %q",
			info.UpstreamSentModel, "claude-sonnet-5")
	}
	if info.UpstreamResponseModel != "claude-sonnet-4-6" {
		t.Fatalf("declared model wrong: %q", info.UpstreamResponseModel)
	}
}

// The snapshot is write-once: a second call must not adopt the upstream's own
// name, which by then has overwritten UpstreamModelName.
func TestSentModelSnapshotIsWriteOnce(t *testing.T) {
	info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "claude-sonnet-5"}}
	info.SnapshotUpstreamSentModel()

	info.UpstreamModelName = "claude-sonnet-4-6" // response-side rewrite
	info.SnapshotUpstreamSentModel()             // must be a no-op

	if info.UpstreamSentModel != "claude-sonnet-5" {
		t.Fatalf("second snapshot overwrote the first: got %q", info.UpstreamSentModel)
	}
}

// A retry re-runs InitChannelMeta before ModelMappedHelper, so the snapshot
// must be cleared per attempt rather than carried over from the previous one.
func TestAttemptResetClearsSentModelSnapshot(t *testing.T) {
	info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "claude-sonnet-5"}}
	info.SnapshotUpstreamSentModel()
	if info.UpstreamSentModel == "" {
		t.Fatal("precondition failed: snapshot not taken")
	}
	info.ResetUpstreamObservation()
	if info.UpstreamSentModel != "" {
		t.Fatalf("snapshot survived the attempt reset: %q", info.UpstreamSentModel)
	}
	// the new attempt can then take a fresh one
	info.UpstreamModelName = "gpt-5.6-sol"
	info.SnapshotUpstreamSentModel()
	if info.UpstreamSentModel != "gpt-5.6-sol" {
		t.Fatalf("fresh snapshot not taken after reset: %q", info.UpstreamSentModel)
	}
}

// ObserveUpstreamResponse must still take the fallback snapshot on paths that
// never run ModelMappedHelper (tests, task relays).
func TestObserveTakesFallbackSnapshotWhenNotFinalised(t *testing.T) {
	info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "gpt-5.6-sol"}}
	info.ObserveUpstreamResponse("gpt-5.6-sol", "resp_1", 0, "")
	if info.UpstreamSentModel != "gpt-5.6-sol" {
		t.Fatalf("fallback snapshot missing: %q", info.UpstreamSentModel)
	}
}

// A nil receiver must not panic — handlers are exercised with bare RelayInfos.
func TestSnapshotUpstreamSentModelNilReceiver(t *testing.T) {
	var info *RelayInfo
	info.SnapshotUpstreamSentModel()
}

// A failed attempt observes an upstream error response; its declared model
// must not be attributed to the channel that serves the retry — especially
// when the retry's response omits the model (nomodel terminal event).
func TestAttemptResetClearsPreviousAttemptObservation(t *testing.T) {
	info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "gpt-5.5", ChannelId: 1}}
	info.SnapshotUpstreamSentModel()
	info.ObserveUpstreamResponse("gpt-5-6-luna", "chatcmpl-first", 1, "fp_a")
	info.ObserveUpstreamUsage(10, 20, 30)
	info.SetUpstreamResponseForensics(`{"error":{"type":"server_error"}}`, map[string][]string{"X-A": {"1"}})

	// attempt 2 on another channel
	info.ChannelMeta = &ChannelMeta{UpstreamModelName: "gpt-5.5", ChannelId: 2}
	info.ResetUpstreamObservation()

	if info.UpstreamSentModel != "" || info.UpstreamResponseModel != "" || info.UpstreamResponseId != "" ||
		info.UpstreamResponseCreated != 0 || info.UpstreamSystemFingerprint != "" ||
		info.UpstreamResponseExcerpt != "" || info.UpstreamResponseHeaders != "" ||
		info.UpstreamInputTokens != 0 || info.UpstreamCacheCreationTokens != 0 || info.UpstreamCacheReadTokens != 0 {
		t.Fatalf("previous attempt's observation leaked into the next attempt: %+v", *info)
	}
	// retry's terminal event omits the model: nothing from attempt 1 may fill it
	info.SnapshotUpstreamSentModel()
	info.ObserveUpstreamResponse("", "resp_0", 0, "")
	if info.UpstreamResponseModel != "" {
		t.Fatalf("model-less retry inherited a model: %q", info.UpstreamResponseModel)
	}
	if info.UpstreamResponseId != "resp_0" {
		t.Fatalf("retry id not recorded: %q", info.UpstreamResponseId)
	}
}

// Excerpts are cut on a rune boundary with NUL bytes and invalid UTF-8 removed
// (PostgreSQL rejects both), and the truncation is recorded with the full size.
func TestForensicsExcerptIsUTF8SafeAndRecordsTruncation(t *testing.T) {
	body := strings.Repeat("a", UpstreamExcerptCap-1) + "海" + "tail"
	info := &RelayInfo{}
	info.SetUpstreamResponseForensicsBytes([]byte(body), nil)
	assert.True(t, strings.HasSuffix(info.UpstreamResponseExcerpt, "a"), "the split rune must be dropped, not mangled")
	assert.LessOrEqual(t, len(info.UpstreamResponseExcerpt), UpstreamExcerptCap)
	assert.True(t, info.UpstreamAudit.ExcerptTruncated)
	assert.Equal(t, len(body), info.UpstreamAudit.ExcerptFullLen)
	assert.True(t, strings.HasSuffix(info.UpstreamAudit.ModelFrame, "海tail"), "an oversized body keeps its tail as the model frame")

	info = &RelayInfo{}
	info.SetUpstreamResponseForensics("ok\x00\xff\xfeend", nil)
	assert.Equal(t, "ok\uFFFDend", info.UpstreamResponseExcerpt)
	assert.False(t, info.UpstreamAudit.ExcerptTruncated)
}

// Headers keep every value, redact credentials, and when over the cap keep
// what fits plus a marker instead of dropping everything.
func TestForensicsHeadersKeepValuesRedactAndCap(t *testing.T) {
	info := &RelayInfo{}
	info.SetUpstreamResponseForensics("", map[string][]string{
		"Via":           {"1.1 a", "1.1 b"},
		"Set-Cookie":    {"session=secret"},
		"Authorization": {"Bearer sk-secret"},
		"X-Api-Key":     {"sk-secret"},
	})
	headers := info.UpstreamResponseHeaders
	assert.Contains(t, headers, `"Via":"1.1 a, 1.1 b"`)
	assert.NotContains(t, headers, "secret")
	assert.Contains(t, headers, `"Set-Cookie":"[redacted]"`)
	assert.False(t, info.UpstreamAudit.HeadersTruncated)

	big := map[string][]string{"A-Small": {"1"}, "B-Huge": {strings.Repeat("x", 9000)}, "C-Small": {"2"}}
	encoded, truncated := EncodeUpstreamAuditHeaders(big)
	assert.True(t, truncated)
	assert.LessOrEqual(t, len(encoded), 8*1024)
	assert.Contains(t, encoded, `"A-Small":"1"`)
	assert.Contains(t, encoded, `"C-Small":"2"`)
	assert.Contains(t, encoded, HeadersTruncatedKey)
}

// A stream records its first model-bearing frame and every distinct model,
// so a usage-only final frame cannot erase the evidence and a mid-stream
// model change is visible.
func TestStreamObservationKeepsModelFrameAndDistinctModels(t *testing.T) {
	info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "gpt-5"}}
	first := `{"id":"c1","model":"gpt-5","choices":[{"delta":{"content":"hi"}}]}`
	info.ObserveUpstreamResponseFrame(first, "gpt-5", "c1", 1, "")
	info.ObserveUpstreamResponseFrame(`{"id":"c1","model":"gpt-5-mini","choices":[]}`, "gpt-5-mini", "c1", 1, "")
	info.ObserveUpstreamResponseFrame(`{"id":"c1","choices":[],"usage":{"total_tokens":3}}`, "", "c1", 1, "")
	info.SetUpstreamResponseForensics(`{"id":"c1","choices":[],"usage":{"total_tokens":3}}`, nil)

	assert.Equal(t, first, info.UpstreamAudit.ModelFrame)
	assert.Equal(t, "gpt-5", info.UpstreamAudit.FirstModel)
	assert.Equal(t, []string{"gpt-5", "gpt-5-mini"}, info.UpstreamAudit.Models)
	assert.Equal(t, "gpt-5-mini", info.UpstreamResponseModel)

	info.ObserveUpstreamErrorFrame(`{"type":"error"}`)
	assert.Equal(t, first, info.UpstreamAudit.ModelFrame, "an error frame must not replace the model frame")
	assert.Equal(t, `{"type":"error"}`, info.UpstreamAudit.ErrorFrame)
}

func TestUpstreamObservationLevel(t *testing.T) {
	info := &RelayInfo{}
	assert.Equal(t, "none", info.UpstreamObservationLevel())
	info.SetUpstreamResponseForensics("", map[string][]string{"X": {"1"}})
	assert.Equal(t, "partial", info.UpstreamObservationLevel())
	info.ObserveUpstreamResponse("gpt-5", "", 0, "")
	assert.Equal(t, "full", info.UpstreamObservationLevel())
	info.ResetUpstreamObservation()
	assert.Equal(t, "none", info.UpstreamObservationLevel())
	require.Empty(t, info.UpstreamAudit.Models)
}

// The output hash ignores whitespace layout, so the same answer streamed in
// different chunkings hashes the same.
func TestObserveUpstreamOutputNormalisesWhitespace(t *testing.T) {
	a, b := &RelayInfo{}, &RelayInfo{}
	a.ObserveUpstreamOutput("Hello,\n  world")
	b.ObserveUpstreamOutput(" Hello, world ")
	assert.Equal(t, a.UpstreamAudit.OutputSHA256, b.UpstreamAudit.OutputSHA256)
	assert.Equal(t, 12, a.UpstreamAudit.OutputChars)
	c := &RelayInfo{}
	c.ObserveUpstreamOutput("   ")
	assert.Empty(t, c.UpstreamAudit.OutputSHA256)
}

// The prompt hash ignores key order and whitespace at every depth, covers the
// system/tools fields, and is empty for a body without prompt fields.
func TestCanonicalPromptSHA256(t *testing.T) {
	a := []byte(`{"model":"x","system":"s","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"f","input_schema":{"b":1,"a":2}}]}`)
	b := []byte(`{ "tools":[{"input_schema":{"a":2,"b":1},"name":"f"}], "messages":[ {"content":[{"text":"hi","type":"text"}],"role":"user"} ], "system":"s","model":"y"}`)
	c := []byte(`{"model":"x","system":"other","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"f","input_schema":{"b":1,"a":2}}]}`)
	assert.Equal(t, CanonicalPromptSHA256(a), CanonicalPromptSHA256(b))
	assert.NotEqual(t, CanonicalPromptSHA256(a), CanonicalPromptSHA256(c), "the system prompt is part of the hash")
	assert.Empty(t, CanonicalPromptSHA256([]byte(`{"model":"x"}`)))
}

type countingReadCloser struct {
	r    io.Reader
	read *int
}

func (c countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.read += n
	return n, err
}

func (c countingReadCloser) Close() error { return nil }

// A large outgoing body is never read back in full at the send boundary: the
// model comes from a bounded prefix and no prompt hash is computed.
func TestSnapshotUpstreamRequestReadsLargeBodiesBoundedly(t *testing.T) {
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"` + strings.Repeat("x", 2<<20) + `"}]}`)
	read := 0
	req, err := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) {
		return countingReadCloser{r: bytes.NewReader(body), read: &read}, nil
	}
	info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "gpt-5-mapped"}}
	info.SnapshotUpstreamRequest(req)

	assert.Equal(t, "gpt-5", info.UpstreamSentModel)
	assert.Empty(t, info.UpstreamAudit.PromptSHA256)
	assert.LessOrEqual(t, read, wireModelPrefixBytes+1)

	small := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	req, err = http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", bytes.NewReader(small))
	require.NoError(t, err)
	info = &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "gpt-5"}}
	info.SnapshotUpstreamRequest(req)
	assert.Equal(t, CanonicalPromptSHA256(small), info.UpstreamAudit.PromptSHA256)
}

// The output hash covers natural-language text only: an agent that repeats an
// identical tool call for different prompts must not look like a replay.
func TestOutputHashExcludesToolCalls(t *testing.T) {
	toolOnly := &RelayInfo{}
	toolOnly.ObserveUpstreamOutputJSON([]byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"function":{"name":"bash","arguments":"{\"command\":\"npm test -- --watch=false --reporter=verbose\"}"}}]}}]}`))
	assert.Empty(t, toolOnly.UpstreamAudit.OutputSHA256)

	claude := &RelayInfo{}
	claude.ObserveUpstreamOutputJSON([]byte(`{"content":[{"type":"text","text":"Running tests."},{"type":"tool_use","name":"bash","input":{"command":"npm test"}}]}`))
	streamed := &RelayInfo{}
	streamed.AppendUpstreamOutputText("Running ")
	streamed.AppendUpstreamOutputText("tests.")
	streamed.FinishUpstreamOutput()
	assert.Equal(t, streamed.UpstreamAudit.OutputSHA256, claude.UpstreamAudit.OutputSHA256)
	assert.Equal(t, 14, claude.UpstreamAudit.OutputChars)
}

// The response id keys the in-memory replay memory, so it is bounded like the
// DB column: an upstream cannot grow memory with huge ids.
func TestObserveUpstreamResponseCapsResponseId(t *testing.T) {
	info := &RelayInfo{}
	info.ObserveUpstreamResponse("gpt-5", strings.Repeat("i", 100_000), 0, "")
	assert.Len(t, info.UpstreamResponseId, 255)
}

// A 70KB body mixing multibyte characters and invalid bytes still fits a
// MySQL TEXT column (65535 bytes) as valid UTF-8: sanitising happens before
// the cut, because each invalid byte grows into a 3-byte U+FFFD.
func TestForensicsExcerptFitsMySQLTextAfterSanitising(t *testing.T) {
	var b strings.Builder
	for b.Len() < 70*1024 {
		b.WriteString("海\xff\xfe")
	}
	info := &RelayInfo{}
	info.SetUpstreamResponseForensicsBytes([]byte(b.String()), nil)
	assert.LessOrEqual(t, len(info.UpstreamResponseExcerpt), 65535)
	assert.True(t, utf8.ValidString(info.UpstreamResponseExcerpt))
	assert.True(t, info.UpstreamAudit.ExcerptTruncated)
	assert.LessOrEqual(t, len(info.UpstreamAudit.ModelFrame), UpstreamFrameCap)
	assert.True(t, utf8.ValidString(info.UpstreamAudit.ModelFrame))

	info = &RelayInfo{}
	info.SetUpstreamResponseForensics(strings.Repeat("a\xff", 30_000), nil)
	// 60 KB of isolated invalid bytes grows to 120 KB and is cut near the cap
	assert.LessOrEqual(t, len(info.UpstreamResponseExcerpt), 65535)
	assert.Greater(t, len(info.UpstreamResponseExcerpt), 65530)
	assert.True(t, info.UpstreamAudit.ExcerptTruncated)
	assert.True(t, utf8.ValidString(info.UpstreamResponseExcerpt))
}

// A body under the cap whose invalid bytes grow past it on replacement is
// reported as truncated, and the system fingerprint is bounded like its column.
func TestForensicsTruncationFlagAndFingerprintBound(t *testing.T) {
	info := &RelayInfo{}
	// each isolated invalid byte becomes a 3-byte U+FFFD: 60 KB grows to 120 KB
	body := []byte(strings.Repeat("a\xff", 30_000))
	require.Less(t, len(body), UpstreamExcerptCap)
	info.SetUpstreamResponseForensicsBytes(body, nil)
	assert.True(t, info.UpstreamAudit.ExcerptTruncated)
	assert.Equal(t, 60_000, info.UpstreamAudit.ExcerptFullLen)
	assert.LessOrEqual(t, len(info.UpstreamResponseExcerpt), UpstreamExcerptCap)

	info.ObserveUpstreamResponse("gpt-5", "chatcmpl-x", 0, strings.Repeat("f", 1000))
	assert.Len(t, info.UpstreamSystemFingerprint, 128)
}

// Vendor fingerprints only count at their protocol-defined position: model
// text, tool arguments, or a Responses event echoing the client's tools /
// metadata / schema can contain the same words.
func TestBodyMarkersRequireProtocolPosition(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  UpstreamBodyMarkers
	}{
		{"text mentions the keys (escaped quotes)",
			`{"id":"chatcmpl-x1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"fields \"prompt_filter_results\", \"content_filters\", \"content_filter_results\", \"prompt_cache_hit_tokens\" and \"prompt_cache_miss_tokens\""}}]}`, 0},
		{"tool argument mentions content_filters",
			`{"id":"chatcmpl-x2","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{\"content_filters\":[{\"content_filter_results\":{}}]}"}}]}}]}`, 0},
		{"Responses echoes a user tool and schema named like the keys",
			`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5","tools":[{"type":"function","name":"content_filters","parameters":{"type":"object","properties":{"content_filters":{"type":"array"},"content_filter_results":{"type":"object"}}}}],"metadata":{"content_filters":"x"},"text":{"format":{"type":"json_schema","schema":{"properties":{"prompt_filter_results":{"type":"array"}}}}}}}`, 0},
		{"unquoted and quoted Bedrock string in model text",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"amazon-bedrock-invocationMetrics and \"amazon-bedrock-invocationMetrics\": {}"}}`, 0},
		{"Azure chat first chunk",
			`{"choices":[],"created":0,"id":"","model":"","object":"","prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}]}`, BodyMarkerAzureContentFilter},
		{"Azure chat first chunk, Asynchronous Filter mode",
			`{"id":"","object":"","created":0,"model":"","prompt_annotations":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}],"choices":[],"usage":null}`, BodyMarkerAzureContentFilter},
		{"Azure chat content chunk",
			`{"choices":[{"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}},"delta":{"content":"Hi"},"index":0}],"id":"chatcmpl-a","model":"gpt-4o"}`, BodyMarkerAzureContentFilter},
		{"Azure Responses completed event",
			`{"type":"response.completed","response":{"id":"resp_2","model":"gpt-5.1","output":[],"content_filters":[{"blocked":false,"source_type":"completion","content_filter_results":{"hate":{"filtered":false,"severity":"safe"}},"content_filter_offsets":{"start_offset":0,"end_offset":6,"check_offset":0}}]}}`, BodyMarkerAzureContentFilter},
		{"Bedrock final event",
			`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":3,"outputTokenCount":2,"invocationLatency":400,"firstByteLatency":200}}`, BodyMarkerBedrockMetrics},
		{"official DeepSeek usage (api.deepseek.com)",
			`{"id":"x","model":"deepseek-chat","choices":[],"usage":{"prompt_tokens":36,"completion_tokens":19,"total_tokens":55,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":17},"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":36}}`, BodyMarkerDeepSeekCacheHit | BodyMarkerDeepSeekCacheMiss},
		{"DeepSeek usage with OpenAI-standard detail subfields a pass-through added",
			`{"id":"x","model":"deepseek-chat","choices":[],"usage":{"prompt_tokens":36,"completion_tokens":19,"total_tokens":55,"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":17,"accepted_prediction_tokens":0},"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":36}}`, BodyMarkerDeepSeekCacheHit | BodyMarkerDeepSeekCacheMiss},
		{"relay union usage with credit (glm-5.3-flash final chunk)",
			`{"id":"chatcmpl-u1","model":"glm-5.3-flash","choices":[],"usage":{"prompt_tokens":926,"completion_tokens":173,"total_tokens":1099,"completion_tokens_details":{"accepted_prediction_tokens":0,"audio_tokens":0,"reasoning_tokens":0,"rejected_prediction_tokens":0,"cached_tokens":0},"prompt_tokens_details":{"accepted_prediction_tokens":0,"audio_tokens":0,"reasoning_tokens":0,"rejected_prediction_tokens":0,"cached_tokens":256},"prompt_cache_hit_tokens":256,"prompt_cache_miss_tokens":670,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"prompt_cache_write_tokens":0,"completion_thinking_tokens":0,"credit":0.02,"cached_tokens":0}}`, 0},
		{"relay union usage without credit (kimi-k3)",
			`{"id":"chatcmpl-u2","model":"kimi-k3","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":40,"total_tokens":160,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":0},"prompt_cache_hit_tokens":64,"prompt_cache_miss_tokens":56,"cache_read_input_tokens":64,"cache_creation_input_tokens":0,"prompt_cache_write_tokens":0,"completion_thinking_tokens":0,"cached_tokens":64}}`, 0},
		{"NewAPI hop usage",
			`{"id":"chatcmpl-h","model":"claude-sonnet-4-5","choices":[],"usage":{"prompt_tokens":3,"claude_cache_creation_5_m_tokens":0}}`, BodyMarkerNewAPICacheSplit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := &RelayInfo{}
			info.ObserveUpstreamBodyMarkers([]byte(c.frame))
			assert.Equal(t, c.want, info.UpstreamAudit.BodyMarkers)
		})
	}
}
