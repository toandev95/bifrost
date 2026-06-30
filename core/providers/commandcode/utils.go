// Package commandcode implements the Command Code AI gateway provider.
//
// Command Code (https://commandcode.ai) is NOT OpenAI-compatible. It exposes a
// single agent-style endpoint, POST /alpha/generate, that always streams a custom
// SSE event format (text-delta / reasoning-delta / tool-call / finish / error).
// This package converts Bifrost's chat schema into Command Code's request shape and
// converts the streamed events back into Bifrost chat responses (both unary and stream).
package commandcode

import (
	"strings"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

const (
	// commandCodeDefaultBaseURL is the upstream API base URL.
	commandCodeDefaultBaseURL = "https://api.commandcode.ai"
	// commandCodeChatPath is the agent generation endpoint.
	commandCodeChatPath = "/alpha/generate"
	// commandCodeModelsPath lists available models.
	commandCodeModelsPath = "/provider/v1/models"
	// commandCodeVersion is sent as the x-command-code-version header. It pins the
	// CLI protocol version expected by the upstream. Keep this in step with the
	// published Command Code CLI to avoid version-gated rejections.
	commandCodeVersion = "0.40.0"
	// maxCommandCodeTokens caps max_tokens to the upstream-accepted ceiling.
	maxCommandCodeTokens = 200_000
)

// commandCodePassthroughFields are params keys that, when present in the request's
// ExtraParams, are forwarded verbatim inside the upstream `params` object. Without
// this pass-through, reasoning/thinking overrides would be silently dropped.
var commandCodePassthroughFields = []string{
	"reasoning_effort",
	"reasoning",
	"thinking",
	"effort",
	"output_config",
	"extra_body",
}

// extractText flattens a Bifrost message content (string or content blocks) into a
// single plain-text string. Non-text blocks (images, audio, files) are ignored —
// Command Code's text endpoint only accepts text.
func extractText(content *schemas.ChatMessageContent) string {
	if content == nil {
		return ""
	}
	if content.ContentStr != nil {
		return *content.ContentStr
	}
	var parts []string
	for _, block := range content.ContentBlocks {
		if block.Type == schemas.ChatContentBlockTypeText && block.Text != nil {
			parts = append(parts, *block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// clampMaxTokens bounds the requested max_tokens into [1, maxCommandCodeTokens].
func clampMaxTokens(requested *int) int {
	if requested == nil {
		return maxCommandCodeTokens
	}
	v := *requested
	if v < 1 {
		return 1
	}
	if v > maxCommandCodeTokens {
		return maxCommandCodeTokens
	}
	return v
}

// mapFinishReason normalizes Command Code finish reasons to Bifrost finish reasons.
func mapFinishReason(reason string) string {
	switch reason {
	case "tool-calls", "tool_calls", "toolUse":
		return string(schemas.BifrostFinishReasonToolCalls)
	case "length", "max_tokens", "max-tokens", "max_output_tokens":
		return string(schemas.BifrostFinishReasonLength)
	default:
		return string(schemas.BifrostFinishReasonStop)
	}
}
