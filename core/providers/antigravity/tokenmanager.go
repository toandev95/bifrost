package antigravity

import (
	"context"
	"sync"
	"time"
)

// cachedToken holds a minted access token and its expiry.
type cachedToken struct {
	accessToken string
	expiresAt   time.Time
}

// tokenManager caches short-lived access tokens keyed by refresh token. The
// durable refresh token lives in the provider Key; access tokens are minted on
// demand and reused until shortly before expiry. This keeps token handling
// self-contained (no DB writes, no coupling to the shared OAuth subsystem).
type tokenManager struct {
	mu    sync.Mutex
	cache map[string]cachedToken
}

func newTokenManager() *tokenManager {
	return &tokenManager{cache: make(map[string]cachedToken)}
}

// expiryGuard is how long before actual expiry we proactively refresh.
const expiryGuard = 2 * time.Minute

// getAccessToken returns a valid access token for the given refresh token,
// refreshing via Google's token endpoint when the cached token is missing or
// near expiry.
func (m *tokenManager) getAccessToken(ctx context.Context, refreshToken string) (string, error) {
	m.mu.Lock()
	if tok, ok := m.cache[refreshToken]; ok && time.Now().Add(expiryGuard).Before(tok.expiresAt) {
		token := tok.accessToken
		m.mu.Unlock()
		return token, nil
	}
	m.mu.Unlock()

	resp, err := refreshAccessToken(ctx, refreshToken)
	if err != nil {
		return "", err
	}

	expiresIn := resp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	m.mu.Lock()
	m.cache[refreshToken] = cachedToken{
		accessToken: resp.AccessToken,
		expiresAt:   time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	m.mu.Unlock()
	return resp.AccessToken, nil
}

// invalidate drops any cached token for a refresh token (e.g. after a 401).
func (m *tokenManager) invalidate(refreshToken string) {
	m.mu.Lock()
	delete(m.cache, refreshToken)
	m.mu.Unlock()
}
