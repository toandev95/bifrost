// Package commandcode implements the Command Code AI gateway provider.
//
// Command Code (https://commandcode.ai) is NOT OpenAI-compatible. It exposes a
// single agent-style endpoint, POST /alpha/generate, that always streams a custom
// SSE event format (text-delta / reasoning-delta / tool-call / finish / error).
// This package converts Bifrost's chat schema into Command Code's request shape and
// converts the streamed events back into Bifrost chat responses (both unary and stream).
package commandcode

import (
	"os"
	"runtime"
	"strings"
	"unicode"

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
	commandCodeVersion = "0.41.1"
	// defaultCommandCodeTokens matches the Command Code CLI default max_tokens.
	defaultCommandCodeTokens = 64_000
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
		return defaultCommandCodeTokens
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

// commandCodeWorkingDir mirrors the CLI's workingDir context as closely as a
// gateway process can. If cwd is unavailable, fall back to the previous neutral
// value instead of failing the request.
func commandCodeWorkingDir() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return "/workspace"
	}
	return wd
}

// commandCodeEnvironment uses the same coarse shape as cmdc:
// "<platform>-<arch>, Node.js <version>". We cannot know the caller's Node
// runtime inside the Go gateway, so the second half identifies Bifrost instead.
func commandCodeEnvironment() string {
	return runtime.GOOS + "-" + runtime.GOARCH + ", Bifrost"
}

// commandCodeProjectSlug mirrors cmdc's slugified cwd project header.
func commandCodeProjectSlug() string {
	slug := slugifyASCII(commandCodeWorkingDir())
	if slug == "" {
		return "bifrost"
	}
	return slug
}

func slugifyASCII(input string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(input) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
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
