package service

import "testing"

func TestUpstreamModelsMatchForAudit(t *testing.T) {
	cases := []struct {
		sent, resp string
		want       bool
	}{
		// exact / case-insensitive
		{"deepseek-v4-pro", "deepseek-v4-pro", true},
		{"GPT-5", "gpt-5", true},
		// tri-state: missing data never flags
		{"", "anything", true},
		{"gpt-5", "", true},
		// OpenAI alias ↔ snapshot
		{"gpt-5", "gpt-5-2025-08-07", true},
		{"gpt-5.2", "gpt-5.2-2025-12-11", true},
		{"gpt-5-2025-08-07", "gpt-5", true}, // reversed
		// DeepSeek date build
		{"deepseek-v4-flash", "deepseek-v4-flash-0731", true},
		// -latest alias
		{"claude-sonnet-4-6", "claude-sonnet-4-6-latest", true},
		// grok runtime build ids
		{"grok-4.5", "grok-4.5-build", true},
		{"grok-4.6-latest", "grok-4.6-build", true},
		// OpenRouter-style vendor/ prefix echoes
		{"deepseek-v4-flash", "deepseek/deepseek-v4-flash", true},
		{"claude-sonnet-4-6", "anthropic/claude-sonnet-4-6", true},
		{"gpt-5", "openai/gpt-5-2025-08-07", true},
		// REAL mismatches must still flag (2026-09-03 #114 real case: 5.5 swapped for 5.6-luna)
		{"gpt-5.5", "gpt-5-6-luna", false},
		{"gpt-5.5", "openai/gpt-5-6-luna", false},
		{"claude-opus-4-7", "deepseek-v4-pro", false},
		{"gpt-5", "gpt-5-mini", false},                    // non-date suffix
		{"deepseek-v4", "deepseek-v4-flash", false},       // different model, prefix relation
		{"gpt-5", "gpt-5-turbo-2024", false},              // mixed suffix
		{"deepseek-v4-pro", "deepseek-v4-flash", false},   // sibling swap
		{"gpt-5", "gpt-5-123456789", false},               // digits too long
	}
	for _, c := range cases {
		if got := UpstreamModelsMatchForAudit(c.sent, c.resp); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.sent, c.resp, got, c.want)
		}
	}
}

func TestInferUpstreamSource(t *testing.T) {
	cases := []struct {
		name        string
		channelType int
		headers     string
		id, model   string
		want        string
	}{
		{"aws by channel type", 33, "", "chatcmpl-x", "claude-x", "aws"},
		{"vertex by channel type", 41, "", "", "gemini-x", "vertex"},
		{"azure by channel type", 3, "", "chatcmpl-x", "gpt-4", "azure"},
		{"azure by apim header", 1, `{"Apim-Request-Id":"x"}`, "chatcmpl-x", "gpt-4", "azure"},
		{"aws by amzn header", 1, `{"X-Amzn-Requestid":"x"}`, "msg_x", "claude-x", "aws"},
		{"openai official", 1, `{"Openai-Organization":"org-x"}`, "chatcmpl-x", "gpt-5", "openai"},
		{"ccmax unified ratelimit", 1, `{"Anthropic-Ratelimit-Unified-Status":"ok"}`, "msg_01x", "claude-x", "ccmax"},
		{"anthropic official", 1, `{"Anthropic-Ratelimit-Requests-Limit":"50"}`, "msg_01x", "claude-x", "anthropic"},
		{"kimi msh header", 1, `{"Msh-Request-Id":"x"}`, "chatcmpl-x", "kimi-k3", "kimi"},
		{"newapi relay", 1, `{"X-New-Api-Version":"1"}`, "chatcmpl-x", "gpt-5", "newapi_relay"},
		{"bedrock id shape", 1, "", "msg_bdrk_abc", "claude-x", "aws"},
		{"claude relay shape", 1, "", "msg_01abc", "claude-x", "claude_relay"},
		{"gpt relay shape", 1, "", "chatcmpl-abc", "gpt-5", "gpt_relay"},
		{"responses api relay shape", 1, "", "resp_0e7981fdbb93b145", "gpt-5.5", "gpt_relay"},
		{"responses api openai official", 1, `{"Openai-Processing-Ms":"123"}`, "resp_0e7981", "gpt-5.5", "openai"},
		{"deepseek uuid", 1, "", "c53e7f1e-873b-41ee-bc8b-810a0d033691", "deepseek-v4-flash", "deepseek"},
		{"deepseek chatcmpl relay", 1, "", "chatcmpl-abc", "deepseek-v4-pro", "deepseek"},
		{"glm numeric id", 1, "", "20260901123456789", "glm-5", "glm"},
		{"unknown", 1, "", "weird-123", "mystery", "unknown"},
		{"empty everything", 1, "", "", "", "unknown"},
	}
	for _, c := range cases {
		if got := InferUpstreamSource(c.channelType, c.headers, c.id, c.model); got != c.want {
			t.Errorf("%s: InferUpstreamSource = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTrackSourceFlip(t *testing.T) {
	ch := 987654 // avoid clashing with other tests
	// first observation seeds silently
	if _, flipped := trackSourceFlip(ch, "azure"); flipped {
		t.Error("first observation must not flip")
	}
	// same source: no flip
	if _, flipped := trackSourceFlip(ch, "azure"); flipped {
		t.Error("same source must not flip")
	}
	// flip azure -> gpt_relay
	from, flipped := trackSourceFlip(ch, "gpt_relay")
	if !flipped || from != "azure" {
		t.Errorf("expected flip from azure, got from=%q flipped=%v", from, flipped)
	}
	// flip back gpt_relay -> azure
	from, flipped = trackSourceFlip(ch, "azure")
	if !flipped || from != "gpt_relay" {
		t.Errorf("expected flip back, got from=%q flipped=%v", from, flipped)
	}
	// repeat flip azure -> gpt_relay: no rate limiting — every flip reported
	from, flipped = trackSourceFlip(ch, "gpt_relay")
	if !flipped || from != "azure" {
		t.Errorf("expected repeat flip reported, got from=%q flipped=%v", from, flipped)
	}
	// unknown never participates
	if _, flipped := trackSourceFlip(ch, "unknown"); flipped {
		t.Error("unknown must not flip")
	}
}
