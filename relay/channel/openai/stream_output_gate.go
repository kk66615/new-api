package openai

import (
	"strings"

	"github.com/tidwall/gjson"
)

// maxHeldStreamBytes bounds the frames an outputGate keeps from the client.
// Frames without output are small; past this size the stream is let through.
const maxHeldStreamBytes = 8 << 20

// outputGate keeps chat stream frames that carry no output (role, usage,
// blank or error frames) from the client until the first frame that does.
// When the stream ends before any output, nothing has been written yet, so
// the attempt can fail without being billed and another channel can answer.
// Reasoning counts as output and streams at once.
type outputGate struct {
	held []string
	size int
	open bool
}

// Pass takes the frame about to be written to the client (empty when there is
// none yet) and the frame just received, and returns the frames to write now,
// in stream order.
func (g *outputGate) Pass(write, next string) []string {
	if g.open {
		if write == "" {
			return nil
		}
		return []string{write}
	}
	if write != "" {
		g.held = append(g.held, write)
		g.size += len(write)
	}
	if !openAIStreamFrameCarriesOutput(next) && g.size+len(next) <= maxHeldStreamBytes {
		return nil
	}
	return g.Release()
}

// Release opens the gate and returns the frames it held.
func (g *outputGate) Release() []string {
	g.open = true
	held := g.held
	g.held = nil
	return held
}

// Open reports whether a frame with output, or too much data, has arrived.
func (g *outputGate) Open() bool {
	return g.open
}

// openAIStreamFrameCarriesOutput reports whether a chat completion chunk gives
// the client anything: a finish reason, or a non-blank delta field other than
// role (text, reasoning, tool or function calls, audio, annotations). Usage,
// error and role-only frames carry none. A frame of an unexpected shape counts
// as output, so an upstream with its own format streams as before.
func openAIStreamFrameCarriesOutput(data string) bool {
	if data == "" {
		return false
	}
	if !gjson.Valid(data) {
		return true
	}
	frame := gjson.Parse(data)
	if !frame.IsObject() {
		return true
	}
	choices := frame.Get("choices")
	if !choices.Exists() || choices.Type == gjson.Null {
		return !frame.Get("usage").Exists() && !frame.Get("error").Exists() &&
			frame.Get("object").Str != "chat.completion.chunk"
	}
	if !choices.IsArray() {
		return true
	}
	carries := false
	choices.ForEach(func(_, choice gjson.Result) bool {
		if !choice.IsObject() {
			carries = true
			return false
		}
		choice.ForEach(func(key, value gjson.Result) bool {
			switch key.Str {
			case "index", "logprobs":
				return true
			case "delta":
				if value.IsObject() {
					value.ForEach(func(field, fieldValue gjson.Result) bool {
						if field.Str != "role" && !blankStreamValue(fieldValue) {
							carries = true
						}
						return !carries
					})
					return !carries
				}
			}
			if !blankStreamValue(value) {
				carries = true
			}
			return !carries
		})
		return !carries
	})
	return carries
}

// blankStreamValue reports whether a chunk field holds nothing: null, false,
// a blank string, or an empty array or object.
func blankStreamValue(value gjson.Result) bool {
	switch {
	case value.Type == gjson.Null, value.Type == gjson.False:
		return true
	case value.Type == gjson.String:
		return strings.TrimSpace(value.Str) == ""
	case value.IsArray():
		return len(value.Array()) == 0
	case value.IsObject():
		return len(value.Map()) == 0
	}
	return false
}
