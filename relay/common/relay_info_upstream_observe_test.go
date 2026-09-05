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
