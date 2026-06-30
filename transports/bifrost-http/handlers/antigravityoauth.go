package handlers

import (
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/providers/antigravity"
	schemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// defaultAntigravityRedirectURI is the loopback redirect used for the Google
// OAuth flow. Antigravity uses a Google "installed app" client, which only
// permits loopback redirect URIs. After consent Google redirects the browser
// here; the user copies the resulting code (or the whole URL) back into the
// dashboard. This works whether Bifrost runs locally or on a remote server.
const defaultAntigravityRedirectURI = "http://localhost:8765"

// antigravityStateTTL bounds how long a pending OAuth state is valid.
const antigravityStateTTL = 15 * time.Minute

// AntigravityOAuthHandler manages the Google OAuth "Connect" flow for the
// Antigravity provider. It is fully self-contained (in-memory PKCE state, no
// shared OAuth/MCP machinery) so it cannot affect other providers.
type AntigravityOAuthHandler struct {
	states sync.Map // state(string) -> *antigravityPendingState
}

type antigravityPendingState struct {
	verifier    string
	redirectURI string
	createdAt   time.Time
}

// NewAntigravityOAuthHandler creates a new handler instance.
func NewAntigravityOAuthHandler() *AntigravityOAuthHandler {
	return &AntigravityOAuthHandler{}
}

// RegisterRoutes registers the Antigravity OAuth routes.
func (h *AntigravityOAuthHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.POST("/api/providers/antigravity/oauth/start", lib.ChainMiddlewares(h.start, middlewares...))
	r.POST("/api/providers/antigravity/oauth/exchange", lib.ChainMiddlewares(h.exchange, middlewares...))
}

type antigravityStartRequest struct {
	RedirectURI string `json:"redirect_uri"`
}

type antigravityStartResponse struct {
	State        string `json:"state"`
	AuthorizeURL string `json:"authorize_url"`
	RedirectURI  string `json:"redirect_uri"`
}

type antigravityExchangeRequest struct {
	State       string `json:"state"`
	Code        string `json:"code"`
	RedirectURI string `json:"redirect_uri"`
}

type antigravityExchangeResponse struct {
	Email      string `json:"email"`
	ProjectID  string `json:"project_id"`
	Credential string `json:"credential"`
}

// start begins a new OAuth flow: it generates PKCE + state and returns the
// Google authorization URL for the browser to open.
func (h *AntigravityOAuthHandler) start(ctx *fasthttp.RequestCtx) {
	var req antigravityStartRequest
	if body := ctx.PostBody(); len(body) > 0 {
		_ = sonic.Unmarshal(body, &req)
	}
	redirectURI := strings.TrimSpace(req.RedirectURI)
	if redirectURI == "" {
		redirectURI = defaultAntigravityRedirectURI
	}

	verifier, challenge, err := antigravity.GeneratePKCE()
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to generate PKCE: "+err.Error())
		return
	}
	state, err := antigravity.GenerateState()
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to generate state: "+err.Error())
		return
	}

	h.gc()
	h.states.Store(state, &antigravityPendingState{
		verifier:    verifier,
		redirectURI: redirectURI,
		createdAt:   time.Now(),
	})

	SendJSON(ctx, antigravityStartResponse{
		State:        state,
		AuthorizeURL: antigravity.BuildAuthURL(redirectURI, state, challenge),
		RedirectURI:  redirectURI,
	})
}

// exchange completes the flow: it exchanges the authorization code, bootstraps
// the Cloud Code project, and returns a credential blob for the UI to store as
// the provider key value.
func (h *AntigravityOAuthHandler) exchange(ctx *fasthttp.RequestCtx) {
	var req antigravityExchangeRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid request body")
		return
	}
	req.State = strings.TrimSpace(req.State)
	code := extractAuthCode(req.Code)
	if req.State == "" || code == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "missing state or code")
		return
	}

	value, ok := h.states.LoadAndDelete(req.State)
	if !ok {
		SendError(ctx, fasthttp.StatusBadRequest, "unknown or expired OAuth state; restart the connection")
		return
	}
	pending := value.(*antigravityPendingState)
	if time.Since(pending.createdAt) > antigravityStateTTL {
		SendError(ctx, fasthttp.StatusBadRequest, "OAuth state expired; restart the connection")
		return
	}

	redirectURI := pending.redirectURI
	if strings.TrimSpace(req.RedirectURI) != "" {
		redirectURI = strings.TrimSpace(req.RedirectURI)
	}

	result, err := antigravity.CompleteAuthorization(ctx, code, redirectURI, pending.verifier)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadGateway, "failed to complete authorization: "+err.Error())
		return
	}

	SendJSON(ctx, antigravityExchangeResponse{
		Email:      result.Email,
		ProjectID:  result.ProjectID,
		Credential: result.Credential,
	})
}

// gc drops expired pending states.
func (h *AntigravityOAuthHandler) gc() {
	now := time.Now()
	h.states.Range(func(k, v any) bool {
		if ps, ok := v.(*antigravityPendingState); ok && now.Sub(ps.createdAt) > antigravityStateTTL {
			h.states.Delete(k)
		}
		return true
	})
}

// extractAuthCode accepts either a raw authorization code or a full redirect URL
// (e.g. "http://localhost:8765/?code=...&scope=...") and returns the code.
func extractAuthCode(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	if strings.Contains(input, "code=") || strings.HasPrefix(input, "http") {
		if u, err := url.Parse(input); err == nil {
			if c := u.Query().Get("code"); c != "" {
				return c
			}
		}
	}
	return input
}
