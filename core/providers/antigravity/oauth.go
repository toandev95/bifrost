package antigravity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oauthHTTPClient is a shared client for OAuth/bootstrap (control-plane) calls.
var oauthHTTPClient = &http.Client{Timeout: 30 * time.Second}

// GeneratePKCE returns a (verifier, challenge) pair for the OAuth PKCE flow.
func GeneratePKCE() (verifier string, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// GenerateState returns a random CSRF state token.
func GenerateState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// BuildAuthURL builds the Google authorization URL for the Antigravity client.
func BuildAuthURL(redirectURI, state, codeChallenge string) string {
	params := url.Values{}
	params.Set("client_id", antigravityClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", strings.Join(antigravityScopes, " "))
	params.Set("state", state)
	params.Set("access_type", "offline")
	params.Set("prompt", "consent")
	if codeChallenge != "" {
		params.Set("code_challenge", codeChallenge)
		params.Set("code_challenge_method", "S256")
	}
	return googleAuthURL + "?" + params.Encode()
}

// ExchangeCode exchanges an authorization code for OAuth tokens.
func ExchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", antigravityClientID)
	form.Set("client_secret", antigravityClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	return postToken(ctx, form)
}

// refreshAccessToken exchanges a refresh token for a fresh access token.
func refreshAccessToken(ctx context.Context, refreshToken string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", antigravityClientID)
	form.Set("client_secret", antigravityClientSecret)
	form.Set("refresh_token", refreshToken)
	return postToken(ctx, form)
}

func postToken(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", nativeOAuthUserAgent())

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("failed to parse token response (status %d): %w", resp.StatusCode, err)
	}
	if tok.Error != "" {
		return &tok, fmt.Errorf("oauth token error: %s: %s", tok.Error, tok.ErrorDesc)
	}
	if resp.StatusCode != http.StatusOK {
		return &tok, fmt.Errorf("oauth token request failed with status %d: %s", resp.StatusCode, string(body))
	}
	return &tok, nil
}

// GetUserInfo fetches the Google account email for an access token.
func GetUserInfo(ctx context.Context, accessToken string) (*userInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleUserInfoURL+"?alt=json", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo request failed with status %d", resp.StatusCode)
	}
	var info userInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// callCodeAssist POSTs a JSON body to a v1internal endpoint, trying each base
// URL in order until one returns 2xx.
func callCodeAssist(ctx context.Context, endpoint, accessToken string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, base := range antigravityBaseURLs {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1internal:"+endpoint, bytes.NewReader(payload))
		if reqErr != nil {
			lastErr = reqErr
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("User-Agent", desktopUserAgent())
		req.Header.Set("Client-Metadata", clientMetadataHeader())

		resp, doErr := oauthHTTPClient.Do(req)
		if doErr != nil {
			lastErr = doErr
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}
		lastErr = fmt.Errorf("%s failed with status %d: %s", endpoint, resp.StatusCode, string(respBody))
	}
	return nil, lastErr
}

// LoadCodeAssist resolves the Cloud Code project id and tier for an account.
func LoadCodeAssist(ctx context.Context, accessToken string) (projectID string, tierID string, err error) {
	body, err := callCodeAssist(ctx, "loadCodeAssist", accessToken, map[string]any{
		"metadata": antigravityLoadCodeAssistMetadata(),
	})
	if err != nil {
		return "", "", err
	}
	var resp loadCodeAssistResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", err
	}
	projectID = extractProjectID(resp.CloudaicompanionProject)
	tierID = "legacy-tier"
	if resp.CurrentTier != nil && resp.CurrentTier.ID != "" {
		tierID = resp.CurrentTier.ID
	} else {
		for _, t := range resp.AllowedTiers {
			if t.IsDefault && t.ID != "" {
				tierID = t.ID
				break
			}
		}
	}
	return projectID, tierID, nil
}

// OnboardUser enables Code Assist for the account and returns the final project
// id. It polls the long-running onboardUser operation until done.
func OnboardUser(ctx context.Context, accessToken, tierID, projectID string) (string, error) {
	for i := 0; i < 6; i++ {
		body, err := callCodeAssist(ctx, "onboardUser", accessToken, map[string]any{
			"tier_id":  tierID,
			"metadata": antigravityLoadCodeAssistMetadata(),
		})
		if err != nil {
			return projectID, err
		}
		var resp onboardUserResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return projectID, err
		}
		if resp.Done {
			if resp.Response != nil {
				if id := extractProjectID(resp.Response.CloudaicompanionProject); id != "" {
					return id, nil
				}
			}
			return projectID, nil
		}
		select {
		case <-ctx.Done():
			return projectID, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return projectID, nil
}

// AuthResult is the outcome of a completed Antigravity OAuth flow. Credential
// is the JSON blob to store as the provider Key value.
type AuthResult struct {
	Email      string `json:"email"`
	ProjectID  string `json:"project_id"`
	Credential string `json:"credential"`
}

// CompleteAuthorization runs the full post-callback bootstrap: exchange code,
// fetch email, resolve + onboard the project. It returns an AuthResult whose
// Credential is ready to be stored as a provider Key value.
func CompleteAuthorization(ctx context.Context, code, redirectURI, codeVerifier string) (*AuthResult, error) {
	tok, err := ExchangeCode(ctx, code, redirectURI, codeVerifier)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token returned (ensure access_type=offline and prompt=consent)")
	}

	cred := &antigravityCredential{RefreshToken: tok.RefreshToken}

	if info, infoErr := GetUserInfo(ctx, tok.AccessToken); infoErr == nil {
		cred.Email = info.Email
	}

	projectID, tierID, lcaErr := LoadCodeAssist(ctx, tok.AccessToken)
	if lcaErr != nil {
		return nil, fmt.Errorf("loadCodeAssist failed: %w", lcaErr)
	}
	if projectID != "" {
		if finalID, onErr := OnboardUser(ctx, tok.AccessToken, tierID, projectID); onErr == nil && finalID != "" {
			projectID = finalID
		}
	}
	if projectID == "" {
		return nil, fmt.Errorf("no Cloud Code project found for this Google account; ensure Gemini Code Assist onboarding is complete")
	}
	cred.ProjectID = projectID

	blob, err := json.Marshal(cred)
	if err != nil {
		return nil, err
	}
	return &AuthResult{Email: cred.Email, ProjectID: cred.ProjectID, Credential: string(blob)}, nil
}
