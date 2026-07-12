package commandcode

import "encoding/json"

// commandCodeRequest is the top-level body sent to POST /alpha/generate.
// Field set and order mirror the real Command Code CLI request:
// {config, memory, taste, skills, permissionMode, params, threadId}.
type commandCodeRequest struct {
	Config         commandCodeConfig      `json:"config"`
	Memory         string                 `json:"memory"`
	Taste          interface{}            `json:"taste"` // null when no taste profile (matches CLI)
	Skills         interface{}            `json:"skills"`
	PermissionMode string                 `json:"permissionMode"`
	Params         map[string]interface{} `json:"params"`
	ThreadID       string                 `json:"threadId"`
}

// commandCodeConfig is the agent execution context. Bifrost runs as an external,
// stateless caller so these are fixed defaults.
type commandCodeConfig struct {
	WorkingDir    string        `json:"workingDir"`
	Date          string        `json:"date"`
	Environment   string        `json:"environment"`
	Structure     []interface{} `json:"structure"`
	IsGitRepo     bool          `json:"isGitRepo"`
	CurrentBranch string        `json:"currentBranch"`
	MainBranch    string        `json:"mainBranch"`
	GitStatus     string        `json:"gitStatus"`
	RecentCommits []interface{} `json:"recentCommits"`
}

// commandCodeMessage is a single converted message. Content is either a plain
// string (user/system) or a slice of typed content parts (assistant/tool).
type commandCodeMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

// commandCodeToolCallPart is an assistant tool-call content part.
type commandCodeToolCallPart struct {
	Type       string                 `json:"type"` // "tool-call"
	ToolCallID string                 `json:"toolCallId"`
	ToolName   string                 `json:"toolName"`
	Input      map[string]interface{} `json:"input"`
}

// commandCodeTextPart is an assistant text content part.
type commandCodeTextPart struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// commandCodeToolResultPart is a tool-result content part.
type commandCodeToolResultPart struct {
	Type       string                   `json:"type"` // "tool-result"
	ToolCallID string                   `json:"toolCallId"`
	ToolName   string                   `json:"toolName"`
	Output     commandCodeToolResultOut `json:"output"`
}

type commandCodeToolResultOut struct {
	Type  string `json:"type"` // "text"
	Value string `json:"value"`
}

// commandCodeTool is a converted tool definition.
type commandCodeTool struct {
	Type        string      `json:"type"` // "function"
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}

// commandCodeStreamEvent is a single SSE event from /alpha/generate.
type commandCodeStreamEvent struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	ToolCallID   string            `json:"toolCallId"`
	ID           string            `json:"id"`
	ToolName     string            `json:"toolName"`
	Name         string            `json:"name"`
	Input        json.RawMessage   `json:"input"`
	Args         json.RawMessage   `json:"args"`
	Arguments    json.RawMessage   `json:"arguments"`
	FinishReason string            `json:"finishReason"`
	TotalUsage   *commandCodeUsage `json:"totalUsage"`
	Error        *commandCodeError `json:"error"`
}

// commandCodeUsage carries token accounting from the finish event.
type commandCodeUsage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	InputTokenDetails struct {
		CacheReadTokens int `json:"cacheReadTokens"`
	} `json:"inputTokenDetails"`
}

// commandCodeError is the body of an HTTP error response or an error stream event.
type commandCodeError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// commandCodeErrorResponse is the shape parsed from non-200 HTTP error bodies.
type commandCodeErrorResponse struct {
	Error   *commandCodeError `json:"error"`
	Message string            `json:"message"`
}
