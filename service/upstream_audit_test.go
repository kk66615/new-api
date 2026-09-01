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
		// REAL mismatches must still flag
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
