// Package antigravity implements the Antigravity (Google Code Assist) provider.
//
// Antigravity is Google's Gemini-powered IDE. Its language server talks to
// Google's internal Cloud Code Assist API at cloudcode-pa.googleapis.com using
// the "v1internal" surface. The inference endpoint is
//
//	POST https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse
//
// where the body is a thin envelope wrapping a standard Gemini generateContent
// request:
//
//	{ "project": "<gcp project>", "requestId": "<uuid>", "model": "<model>",
//	  "userAgent": "...", "requestType": "agent",
//	  "request": { <Gemini GenerateContentRequest> } }
//
// and each streamed chunk is wrapped as { "response": { <GenerateContentResponse> } }.
//
// Auth is Google OAuth2 (consumer/desktop client, public client_id/secret).
// Bifrost stores the durable refresh token (plus the resolved Cloud Code
// project id and account email) as the provider Key value (a JSON blob) and
// refreshes the short-lived access token on demand.
//
// This package reuses the Gemini provider's request/response converters
// (gemini.ToGeminiChatCompletionRequest, GenerateContentResponse.ToBifrost*)
// so it inherits full tool-calling, vision and reasoning support without
// duplicating the Gemini schema.
package antigravity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// antigravityClientID is the public Google OAuth client id shipped in the
	// Antigravity desktop app. Google documents that native/installed-app OAuth
	// client_id/secret using PKCE/loopback are public and not secret.
	antigravityClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	// antigravityClientSecret is the matching public client secret.
	antigravityClientSecret = "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"

	// Google OAuth endpoints.
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://www.googleapis.com/oauth2/v1/userinfo"

	// Client fingerprint. These match the native Antigravity desktop client and
	// must stay consistent across headers + envelope to avoid looking like a
	// non-native (proxied) request. Bump antigravityVersion to track upstream.
	antigravityVersion = "4.2.0"
	chromeVersion      = "132.0.6834.160"
	electronVersion    = "39.2.3"

	// requestTypeAgent is the default requestType for chat/agent inference.
	requestTypeAgent = "agent"

	// maxAntigravityOutputTokens caps generationConfig.maxOutputTokens to match
	// the native client and avoid upstream 400s on oversized values.
	maxAntigravityOutputTokens = 16384
)

// vscodeSessionID is a stable per-process identifier sent as x-vscode-sessionid,
// mirroring the native client which generates one UUID per editor session.
var vscodeSessionID = uuid.NewString()

// antigravityBaseURLs are the Cloud Code Assist hosts, tried in order. The
// stable production host is first; the daily channel is a fallback.
var antigravityBaseURLs = []string{
	"https://cloudcode-pa.googleapis.com",
	"https://daily-cloudcode-pa.googleapis.com",
}

// antigravityScopes are the OAuth scopes requested during the browser flow.
var antigravityScopes = []string{
	"openid",
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// defaultSafetySettings mirrors the native client's all-OFF safety configuration.
// Sent in the inner request when the caller did not specify safety settings, so
// benign technical prompts are not server-side flagged (and to match fingerprint).
// Note: CIVIC_INTEGRITY is intentionally omitted — the Cloud Code endpoint
// rejects it with a 400 "element predicate failed".
func defaultSafetySettings() []map[string]string {
	return []map[string]string{
		{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "OFF"},
	}
}

// antigravityModelAliases maps friendly/public model ids to the upstream ids
// accepted by Cloud Code (mirrors Antigravity's own model selector). Some Pro
// ids are remapped because Google renamed them upstream (the IDE-facing id now
// 400s while the agent id works).
var antigravityModelAliases = map[string]string{
	"gemini-3-pro-preview":     "gemini-3.1-pro",
	"gemini-3.1-pro-high":      "gemini-pro-agent", // renamed upstream; old id 400s
	"gemini-3.5-flash-low":     "gemini-3.5-flash-extra-low",
	"gemini-3.5-flash-medium":  "gemini-3.5-flash-low",
	"gemini-3.5-flash-high":    "gemini-3-flash-agent",
	"gemini-3.5-flash-preview": "gemini-3-flash-agent",
}

// resolveModelAlias maps a (already prefix-stripped) model id to its upstream id.
func resolveModelAlias(model string) string {
	if up, ok := antigravityModelAliases[model]; ok {
		return up
	}
	return model
}

// antigravityLoadCodeAssistMetadata is the metadata object sent with
// loadCodeAssist / onboardUser. Matches the native client (ideType only).
func antigravityLoadCodeAssistMetadata() map[string]string {
	return map[string]string{"ideType": "ANTIGRAVITY"}
}

// clientMetadataHeader is the JSON-encoded Client-Metadata header value sent on
// bootstrap (loadCodeAssist/onboardUser) calls by the native ide client.
func clientMetadataHeader() string {
	b, _ := json.Marshal(antigravityLoadCodeAssistMetadata())
	return string(b)
}

// platformUAToken returns the OS token used in the desktop User-Agent, based on
// the host running Bifrost (the proxy), matching what a native client on that
// OS would send.
func platformUAToken() string {
	switch runtime.GOOS {
	case "darwin":
		return "Macintosh; Intel Mac OS X 10_15_7"
	case "windows":
		return "Windows NT 10.0; Win64; x64"
	default:
		return "X11; Linux x86_64"
	}
}

// desktopUserAgent is the Antigravity Electron desktop User-Agent used for
// content (inference) and bootstrap (loadCodeAssist/onboardUser) calls.
func desktopUserAgent() string {
	return fmt.Sprintf("Antigravity/%s (%s) Chrome/%s Electron/%s", antigravityVersion, platformUAToken(), chromeVersion, electronVersion)
}

// nativeOAuthUserAgent is the User-Agent used for OAuth token endpoint calls
// (code exchange + refresh), matching the native client.
func nativeOAuthUserAgent() string {
	return "vscode/1.X.X (Antigravity/" + antigravityVersion + ")"
}

// envelopeUserAgent returns the envelope-level userAgent tag the native client
// sends: "antigravity" for consumer (gmail) accounts, "jetski" for enterprise.
func envelopeUserAgent(email string) string {
	e := strings.ToLower(strings.TrimSpace(email))
	if e != "" && !strings.HasSuffix(e, "@gmail.com") && !strings.HasSuffix(e, "@googlemail.com") {
		return "jetski"
	}
	return "antigravity"
}

// generateRequestID produces a request id in the native format: agent/<ms>/<hex>.
func generateRequestID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("agent/%d/%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

// deriveSessionID derives a stable session id from the account email using the
// same FNV-1a (64-bit, signed) hash as the native client, so a given account
// consistently reuses one session id. Falls back to a random value.
func deriveSessionID(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return "-0"
		}
		var v uint64
		for _, b := range buf {
			v = (v << 8) | uint64(b)
		}
		return "-" + strconv.FormatUint(v%9_000_000_000_000_000_000, 10)
	}
	var hash int64 = -3750763034362895579 // FNV-1a 64-bit offset basis (signed)
	const prime int64 = 1099511628211
	for _, b := range []byte(email) {
		hash ^= int64(b)
		hash *= prime // signed two's-complement wraparound matches BigInt.asIntN(64)
	}
	return strconv.FormatInt(hash, 10)
}

// normalizeModelName strips an optional "antigravity/" routing prefix so the
// envelope carries the bare upstream model id (e.g. "gemini-2.5-pro").
func normalizeModelName(model string) string {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "antigravity/")
	return resolveModelAlias(model)
}

// parseCredential parses the Key value JSON blob into an antigravityCredential.
// For backward tolerance, a bare (non-JSON) value is treated as a raw refresh
// token with no project id.
func parseCredential(value string) antigravityCredential {
	value = strings.TrimSpace(value)
	var cred antigravityCredential
	if value == "" {
		return cred
	}
	if strings.HasPrefix(value, "{") {
		_ = json.Unmarshal([]byte(value), &cred)
		return cred
	}
	cred.RefreshToken = value
	return cred
}

// extractProjectID resolves a project id from a cloudaicompanionProject field
// that may be either a JSON string or an object with an "id".
func extractProjectID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj cloudaicompanionProject
	if err := json.Unmarshal(raw, &obj); err == nil {
		return strings.TrimSpace(obj.ID)
	}
	return ""
}
