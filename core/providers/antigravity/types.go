package antigravity

import "encoding/json"

// antigravityCredential is the JSON blob stored as the provider Key value. It
// holds the durable OAuth refresh token plus the resolved Cloud Code project id
// and account email. The short-lived access token is never persisted — it is
// minted on demand from the refresh token and cached in memory.
type antigravityCredential struct {
	RefreshToken string `json:"refresh_token"`
	ProjectID    string `json:"project_id"`
	Email        string `json:"email,omitempty"`
}

// requestEnvelope is the Cloud Code Assist wrapper around a Gemini request.
// Field order mirrors the native client. The Request field carries the raw
// Gemini GenerateContentRequest JSON produced by the Gemini converter.
type requestEnvelope struct {
	Project            string          `json:"project"`
	RequestID          string          `json:"requestId"`
	Request            json.RawMessage `json:"request"`
	Model              string          `json:"model"`
	UserAgent          string          `json:"userAgent,omitempty"`
	RequestType        string          `json:"requestType"`
	EnabledCreditTypes []string        `json:"enabledCreditTypes,omitempty"`
}

// streamChunk is the per-event wrapper returned by streamGenerateContent. The
// inner Response is a standard Gemini GenerateContentResponse.
type streamChunk struct {
	Response json.RawMessage `json:"response"`
}

// tokenResponse is the Google OAuth token endpoint response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// userInfo is the Google userinfo response (only the email is used).
type userInfo struct {
	Email string `json:"email"`
}

// cloudaicompanionProject models the loadCodeAssist project field, which may be
// either a bare string id or an object with an "id".
type cloudaicompanionProject struct {
	ID string `json:"id"`
}

// loadCodeAssistResponse is the response from v1internal:loadCodeAssist.
type loadCodeAssistResponse struct {
	CloudaicompanionProject json.RawMessage  `json:"cloudaicompanionProject"`
	CurrentTier             *codeAssistTier  `json:"currentTier"`
	AllowedTiers            []codeAssistTier `json:"allowedTiers"`
}

type codeAssistTier struct {
	ID        string `json:"id"`
	IsDefault bool   `json:"isDefault"`
}

// onboardUserResponse is the response from v1internal:onboardUser (long-running
// operation style: done + response.cloudaicompanionProject).
type onboardUserResponse struct {
	Done     bool `json:"done"`
	Response *struct {
		CloudaicompanionProject json.RawMessage `json:"cloudaicompanionProject"`
	} `json:"response"`
}
