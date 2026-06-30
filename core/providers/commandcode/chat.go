package commandcode

import (
	"encoding/json"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	schemas "github.com/maximhq/bifrost/core/schemas"
)

// buildCommandCodeRequest converts a Bifrost chat request into Command Code's
// agent request body. The upstream always streams, so Params.stream is always true;
// the unary path aggregates the stream into a full response internally.
func buildCommandCodeRequest(request *schemas.BifrostChatRequest) *commandCodeRequest {
	system, messages := convertMessages(request.Input)

	var tools []commandCodeTool
	var maxTokens *int
	extra := map[string]interface{}{}
	var temperature *float64
	if request.Params != nil {
		tools = convertTools(request.Params.Tools)
		maxTokens = request.Params.MaxCompletionTokens
		temperature = request.Params.Temperature
		// Carry forward whitelisted passthrough fields from ExtraParams.
		for _, field := range commandCodePassthroughFields {
			if v, ok := request.Params.ExtraParams[field]; ok && v != nil {
				extra[field] = v
			}
		}
	}
	if tools == nil {
		tools = []commandCodeTool{}
	}

	params := map[string]interface{}{
		"model":      request.Model,
		"messages":   messages,
		"tools":      tools,
		"system":     system,
		"max_tokens": clampMaxTokens(maxTokens),
		"stream":     true,
	}
	if temperature != nil {
		params["temperature"] = *temperature
	}
	for k, v := range extra {
		params[k] = v
	}

	return &commandCodeRequest{
		Config: commandCodeConfig{
			WorkingDir:    "/workspace",
			Date:          time.Now().UTC().Format("2006-01-02"),
			Environment:   "external",
			Structure:     []interface{}{},
			IsGitRepo:     false,
			RecentCommits: []interface{}{},
		},
		Taste:    nil,
		Params:   params,
		ThreadID: uuid.NewString(),
	}
}

// convertUserContent maps Bifrost user-message content into the shape Command Code
// accepts for user messages: a plain string for text-only, or an OpenAI-style content
// part array (text + image_url + input_audio) for multimodal/vision input. Returning
// the OpenAI shape (rather than flattening) is what preserves vision support.
func convertUserContent(content *schemas.ChatMessageContent) interface{} {
	if content == nil {
		return ""
	}
	if content.ContentStr != nil {
		return *content.ContentStr
	}
	parts := make([]interface{}, 0, len(content.ContentBlocks))
	for i := range content.ContentBlocks {
		block := &content.ContentBlocks[i]
		switch block.Type {
		case schemas.ChatContentBlockTypeText:
			if block.Text != nil {
				parts = append(parts, map[string]interface{}{"type": "text", "text": *block.Text})
			}
		case schemas.ChatContentBlockTypeImage:
			if block.ImageURLStruct != nil {
				// Command Code uses AI-SDK content parts: {type:"image", image:<url|data-url>}.
				// The URL may be an https link or a "data:<mime>;base64,..." string; both are
				// passed through as the single `image` string field.
				parts = append(parts, map[string]interface{}{"type": "image", "image": block.ImageURLStruct.URL})
			}
		}
	}
	if len(parts) == 0 {
		// Fall back to flattened text (or empty) so we never send an empty array.
		return extractText(content)
	}
	return parts
}

// convertMessages splits Bifrost messages into a joined system prompt and the
// Command Code message list. Tool calls are only emitted when paired with a result
// (and vice versa) to satisfy the upstream's strict pairing requirement.
func convertMessages(input []schemas.ChatMessage) (string, []commandCodeMessage) {
	pairedIDs := completeToolCallIDs(input)

	var systemParts []string
	out := make([]commandCodeMessage, 0, len(input))

	for i := range input {
		msg := &input[i]
		switch msg.Role {
		case schemas.ChatMessageRoleSystem, schemas.ChatMessageRoleDeveloper:
			if text := extractText(msg.Content); text != "" {
				systemParts = append(systemParts, text)
			}

		case schemas.ChatMessageRoleUser:
			out = append(out, commandCodeMessage{Role: "user", Content: convertUserContent(msg.Content)})

		case schemas.ChatMessageRoleAssistant:
			parts := []interface{}{}
			if text := extractText(msg.Content); text != "" {
				parts = append(parts, commandCodeTextPart{Type: "text", Text: text})
			}
			if msg.ChatAssistantMessage != nil {
				for _, call := range msg.ChatAssistantMessage.ToolCalls {
					id := ""
					if call.ID != nil {
						id = *call.ID
					}
					if id == "" || !pairedIDs[id] {
						continue
					}
					name := ""
					if call.Function.Name != nil {
						name = *call.Function.Name
					}
					parts = append(parts, commandCodeToolCallPart{
						Type:       "tool-call",
						ToolCallID: id,
						ToolName:   name,
						Input:      parseArguments(call.Function.Arguments),
					})
				}
			}
			if len(parts) > 0 {
				out = append(out, commandCodeMessage{Role: "assistant", Content: parts})
			}

		case schemas.ChatMessageRoleTool:
			id := ""
			if msg.ChatToolMessage != nil && msg.ChatToolMessage.ToolCallID != nil {
				id = *msg.ChatToolMessage.ToolCallID
			}
			if id == "" || !pairedIDs[id] {
				continue
			}
			name := ""
			if msg.Name != nil {
				name = *msg.Name
			}
			out = append(out, commandCodeMessage{
				Role: "tool",
				Content: []interface{}{commandCodeToolResultPart{
					Type:       "tool-result",
					ToolCallID: id,
					ToolName:   name,
					Output:     commandCodeToolResultOut{Type: "text", Value: extractText(msg.Content)},
				}},
			})
		}
	}

	system := ""
	for i, p := range systemParts {
		if i > 0 {
			system += "\n\n"
		}
		system += p
	}
	return system, out
}

// completeToolCallIDs returns the set of tool-call IDs that have BOTH an assistant
// tool_call and a matching tool result.
func completeToolCallIDs(input []schemas.ChatMessage) map[string]bool {
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	for i := range input {
		msg := &input[i]
		if msg.Role == schemas.ChatMessageRoleAssistant && msg.ChatAssistantMessage != nil {
			for _, call := range msg.ChatAssistantMessage.ToolCalls {
				if call.ID != nil && *call.ID != "" {
					callIDs[*call.ID] = true
				}
			}
		}
		if msg.Role == schemas.ChatMessageRoleTool && msg.ChatToolMessage != nil && msg.ChatToolMessage.ToolCallID != nil {
			resultIDs[*msg.ChatToolMessage.ToolCallID] = true
		}
	}
	paired := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			paired[id] = true
		}
	}
	return paired
}

// parseArguments decodes a stringified JSON tool-call argument blob into a map.
// On failure (or empty), returns an empty map so the upstream still receives an object.
func parseArguments(arguments string) map[string]interface{} {
	result := map[string]interface{}{}
	if arguments == "" {
		return result
	}
	if err := sonic.UnmarshalString(arguments, &result); err != nil {
		return map[string]interface{}{}
	}
	return result
}

// convertTools maps Bifrost function tools to Command Code's tool shape.
func convertTools(tools []schemas.ChatTool) []commandCodeTool {
	out := make([]commandCodeTool, 0, len(tools))
	for i := range tools {
		fn := tools[i].Function
		if fn == nil {
			continue
		}
		description := ""
		if fn.Description != nil {
			description = *fn.Description
		}
		out = append(out, commandCodeTool{
			Type:        "function",
			Name:        fn.Name,
			Description: description,
			InputSchema: toCommandCodeInputSchema(fn.Parameters),
		})
	}
	return out
}

// toCommandCodeInputSchema normalizes a tool's parameter schema into a valid JSON
// Schema object. Command Code rejects any tool whose input_schema is not
// `{"type":"object", ...}` (e.g. parameterless tools that arrive with a null/empty
// type), so we always guarantee a "type":"object" with a "properties" object.
func toCommandCodeInputSchema(params *schemas.ToolFunctionParameters) interface{} {
	base := map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	if params == nil {
		return base
	}
	b, err := sonic.Marshal(params)
	if err != nil {
		return base
	}
	var m map[string]interface{}
	if err := sonic.Unmarshal(b, &m); err != nil || len(m) == 0 {
		return base
	}
	if t, ok := m["type"].(string); !ok || t == "" {
		m["type"] = "object"
	}
	if _, ok := m["properties"]; !ok {
		m["properties"] = map[string]interface{}{}
	}
	return m
}

// toolCallArguments returns the stringified JSON of whichever input field the
// tool-call event carries (input | args | arguments).
func (e *commandCodeStreamEvent) toolCallArguments() string {
	for _, raw := range []json.RawMessage{e.Input, e.Args, e.Arguments} {
		if len(raw) > 0 {
			return string(raw)
		}
	}
	return "{}"
}

// usageToBifrost converts Command Code usage into Bifrost usage.
func usageToBifrost(u *commandCodeUsage) *schemas.BifrostLLMUsage {
	if u == nil {
		return nil
	}
	prompt := u.InputTokens + u.InputTokenDetails.CacheReadTokens
	completion := u.OutputTokens
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
	if u.InputTokenDetails.CacheReadTokens > 0 {
		usage.PromptTokensDetails = &schemas.ChatPromptTokensDetails{
			CachedReadTokens: u.InputTokenDetails.CacheReadTokens,
		}
	}
	return usage
}
