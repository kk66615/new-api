#!/bin/bash
# Fork check, run after every upstream merge (from the repo root):
#   bin/fork-checks/check_empty_stream_hooks.sh
#
# An upstream that answers 200 and closes a stream without a single data event
# must fail the attempt (no charge, retry on another channel) instead of being
# settled on the locally estimated prompt tokens. That only holds while every
# response handler running helper.StreamScannerHandler calls
# helper.EmptyUpstreamStreamError right after it. A merge rarely conflicts here;
# the risk is a NEW handler that silently lacks the hook. This script fails if
# any scanner call site has no hook, or if the other parts of the fix are gone.
set -u
export LC_ALL=C # stable sort/comm order whatever the terminal locale
cd "$(git rev-parse --show-toplevel)" || exit 2

# Image and audio streams settle on their own usage rules (see .agents/rules/billing.md).
exempt='relay/channel/openai/relay_image.go|relay/channel/openai/audio.go'
fail=0

for f in $(grep -rl --include='*.go' 'helper.StreamScannerHandler(' relay | grep -v '_test.go' | grep -vE "^($exempt)$" | sort); do
	calls=$(grep -c 'helper.StreamScannerHandler(' "$f")
	hooks=$(grep -c 'helper.EmptyUpstreamStreamError(' "$f")
	if [ "$hooks" -lt "$calls" ]; then
		echo "MISSING hook: $f (StreamScannerHandler calls=$calls, EmptyUpstreamStreamError calls=$hooks)"
		fail=1
	else
		echo "ok   $f"
	fi
done

require() { # file, fixed string, what it protects
	if grep -qF -- "$2" "$1"; then
		echo "ok   $1: $3"
	else
		echo "MISSING in $1: $3 (expected: $2)"
		fail=1
	fi
}
require relay/helper/stream_scanner.go 'info.StreamStatus.BeginScan()' 'scanner owns the data-event count'
require relay/helper/stream_scanner.go 'info.StreamStatus.ObserveDataEvent()' 'scanner counts data events'
require relay/helper/stream_scanner.go 'c.Writer.Header().Del(name)' 'retry does not inherit the failed upstream Codex headers'
require relay/channel/openai/chat_via_responses.go 'helper.EmptyUpstreamStreamError(c, info, resp)' 'buffered Responses-to-chat path (own scanner loop)'
require relay/channel/gemini/relay_responses.go 'info.StreamStatus.UpstreamSentNothing()' 'Gemini-to-Responses answers the failure instead of a response.failed stream'
require relay/channel/openai/relay-openai.go 'if dataFrames == 1 && errorFrame != nil {' 'a chat stream of only an error frame fails instead of billing the prompt estimate'
require controller/relay.go 'relayInfo.IsStream = clientIsStream' 'each retry restores the client request mode'
require service/upstream_audit.go 'emptyStream := failed && relayInfo.StreamStatus.UpstreamSentNothing()' 'empty streams still get an evidence row'
for f in relay/compatible_handler.go relay/claude_handler.go relay/gemini_handler.go relay/image_handler.go; do
	require "$f" 'helper.UpstreamAnswersWithStream(c, httpResp)' 'a JSON answer labelled text/event-stream is not turned into a stream'
done
require relay/chat_completions_via_responses.go 'upstreamStream = helper.UpstreamAnswersWithStream(c, httpResp)' 'same for chat over the Responses API'
# An upstream merge may bring back the bare Content-Type check (channel #148 labels JSON answers event-stream).
raw=$(grep -rn --include='*.go' 'info.IsStream = info.IsStream || strings.HasPrefix(' relay | grep -v '_test.go')
if [ -n "$raw" ]; then
	echo "MISSING sniffing, IsStream widened on Content-Type alone:"
	echo "$raw"
	fail=1
fi

# The refusal reason is shown to every log owner (service/text_quota.go
# refusal_reason), so only upstream response handlers may write it: a new writer
# from an upstream merge could put internal information into a public field.
# (controller/relay.go only clears it at the start of every attempt.)
require service/text_quota.go 'other.SetPublic("refusal_reason", publicRefusalReason(adminRejectReason))' 'refusal reason visible to the log owner, codes only'
# Every line that writes the reason, counted per file: a new writer, even in an
# already reviewed file, changes a count and must be reviewed before release.
writers=$(grep -rn --include='*.go' -e 'ContextKeyAdminRejectReason' -e '"admin_reject_reason"' . | grep -v '_test.go' | grep -v 'GetContextKey' | grep -v '^./constant/context_key.go' | cut -d: -f1 | sed 's|^\./||' | sort | uniq -c | awk '{print $2"="$1}')
expected='controller/relay.go=1
relay/channel/claude/relay-claude.go=1
relay/channel/gemini/relay-gemini-native.go=1
relay/channel/gemini/relay-gemini.go=4
relay/channel/gemini/relay_responses.go=2
relay/channel/openai/relay-openai.go=1'
if [ "$writers" != "$expected" ]; then
	echo "REVIEW writers of the public refusal reason changed (each must record only a code from the upstream response; then update the expected counts):"
	diff <(echo "$expected") <(echo "$writers")
	fail=1
else
	echo "ok   refusal reason written only by the reviewed upstream response handlers"
fi

if [ "$fail" -ne 0 ]; then
	echo "FAIL: empty-stream protection is incomplete; see the lines above"
	exit 1
fi
echo "PASS: every stream scanner call site is protected"
