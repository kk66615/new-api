package common

import "testing"

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

// The snapshot must be taken at the SEND boundary, i.e. after every
// request-side rewrite. This test reproduces the real handler order
// (InitChannelMeta -> ModelMappedHelper -> reasoning/search suffix handling ->
// adaptor ConvertRequest -> doRequest) and asserts the snapshot lands on the
// final name, not the pre-strip one.
//
// This is the case that slipped through the first fix: taking the snapshot in
// ModelMappedHelper recorded "claude-3-7-sonnet-thinking" while the request
// that actually went out carried "claude-3-7-sonnet", so an upstream honestly
// echoing the request was reported as a model swapper.
func TestSnapshotHappensAfterRequestSideRewrites(t *testing.T) {
	cases := []struct {
		name        string
		atSnapshot  string // UpstreamModelName when the snapshot is taken
		preStripped string // an earlier, not-yet-stripped value
		want        string
	}{
		{
			name:        "reasoning suffix stripped before send",
			preStripped: "claude-3-7-sonnet-thinking",
			atSnapshot:  "claude-3-7-sonnet",
			want:        "claude-3-7-sonnet",
		},
		{
			name:        "xai -search suffix stripped before send",
			preStripped: "grok-4.5-search",
			atSnapshot:  "grok-4.5",
			want:        "grok-4.5",
		},
		{
			name:        "reasoning effort suffix stripped before send",
			preStripped: "gpt-5-high",
			atSnapshot:  "gpt-5",
			want:        "gpt-5",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: c.preStripped}}
			info.ResetUpstreamObservation() // what InitChannelMeta does per attempt
			// ModelMappedHelper no longer snapshots; the suffix handling then
			// rewrites the name, and only doRequest freezes it.
			info.UpstreamModelName = c.atSnapshot
			info.SnapshotUpstreamSentModel()
			if info.UpstreamSentModel != c.want {
				t.Fatalf("sent model = %q, want %q (a pre-strip snapshot makes an "+
					"honest upstream look like a swapper)", info.UpstreamSentModel, c.want)
			}
		})
	}
}

// The pairing that matters: after the snapshot, the comparison against the
// model the upstream declares must be clean when the upstream honestly echoes
// the request.
func TestSnapshotMakesHonestEchoCompareClean(t *testing.T) {
	info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "claude-3-7-sonnet"}}
	info.SnapshotUpstreamSentModel()
	info.UpstreamModelName = "claude-3-7-sonnet" // any later rewrite
	if info.UpstreamSentModel != info.UpstreamModelName {
		t.Fatalf("snapshot drifted: %q vs %q", info.UpstreamSentModel, info.UpstreamModelName)
	}
	if info.UpstreamSentModel == "claude-3-7-sonnet-thinking" {
		t.Fatal("snapshot kept a pre-strip suffix")
	}
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
