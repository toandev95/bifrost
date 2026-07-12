package commandcode

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	schemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const (
	commandCodeUsageSummaryPath            = "/alpha/usage/summary"
	commandCodeBillingCreditsPath          = "/alpha/billing/credits"
	commandCodeQuotaDefaultThreshold       = 95.0
	commandCodeQuotaDefaultCacheTTL        = 60 * time.Second
	commandCodeQuotaDefaultTimeout         = 8 * time.Second
	commandCodeQuotaEnvEnabled             = "BIFROST_COMMAND_CODE_QUOTA_PREFLIGHT"
	commandCodeQuotaEnvThreshold           = "BIFROST_COMMAND_CODE_QUOTA_THRESHOLD"
	commandCodeQuotaEnvCacheTTLSeconds     = "BIFROST_COMMAND_CODE_QUOTA_CACHE_TTL_SECONDS"
	commandCodeQuotaEnvFailClosed          = "BIFROST_COMMAND_CODE_QUOTA_FAIL_CLOSED"
	commandCodeQuotaEnvDebugLog            = "BIFROST_COMMAND_CODE_QUOTA_DEBUG_LOG"
	commandCodeQuotaWindowFiveHour         = "five_hour"
	commandCodeQuotaWindowWeekly           = "weekly"
	commandCodeQuotaRateLimitErrorType     = "rate_limit_error"
	commandCodeQuotaRateLimitErrorCode     = "command_code_quota_exhausted"
	commandCodeQuotaLogBodyLimit           = 8192
	commandCodeQuotaMaxCacheKeyTokenLength = 48
)

type commandCodeQuotaConfig struct {
	Enabled          bool
	ThresholdPercent float64
	CacheTTL         time.Duration
	Timeout          time.Duration
	FailClosed       bool
	DebugLog         bool
}

type commandCodeQuotaEntry struct {
	Snapshot  commandCodeQuotaSnapshot
	FetchedAt time.Time
}

type commandCodeQuotaSnapshot struct {
	PercentUsed  float64
	ResetAt      time.Time
	LimitReached bool
	Windows      map[string]commandCodeQuotaWindow
}

type commandCodeQuotaWindow struct {
	Used        float64
	Cap         float64
	PercentUsed float64
	ResetAt     time.Time
	Exceeded    bool
}

type commandCodeQuotaCacheState int

const (
	commandCodeQuotaCacheMiss commandCodeQuotaCacheState = iota
	commandCodeQuotaCacheFresh
	commandCodeQuotaCacheStale
)

func newCommandCodeQuotaConfig() commandCodeQuotaConfig {
	return commandCodeQuotaConfig{
		Enabled:          commandCodeEnvBool(commandCodeQuotaEnvEnabled, true),
		ThresholdPercent: commandCodeEnvFloat(commandCodeQuotaEnvThreshold, commandCodeQuotaDefaultThreshold),
		CacheTTL:         time.Duration(commandCodeEnvInt(commandCodeQuotaEnvCacheTTLSeconds, int(commandCodeQuotaDefaultCacheTTL.Seconds()))) * time.Second,
		Timeout:          commandCodeQuotaDefaultTimeout,
		FailClosed:       commandCodeEnvBool(commandCodeQuotaEnvFailClosed, false),
		DebugLog:         commandCodeEnvBool(commandCodeQuotaEnvDebugLog, false),
	}
}

func (p *commandCodeProvider) preflightQuota(apiKey string, key schemas.Key, model string) *schemas.BifrostError {
	if !p.quotaConfig.Enabled {
		return nil
	}

	cacheKey := p.quotaCacheKey(apiKey, key, model)
	cached, cacheState := p.getCachedQuota(cacheKey)
	if cacheState == commandCodeQuotaCacheFresh {
		if err := p.quotaBlockedError(cached); err != nil {
			return err
		}
		return nil
	}

	snapshot, fetchErr := p.fetchQuota(apiKey)
	if fetchErr != nil {
		if cacheState == commandCodeQuotaCacheStale {
			if err := p.quotaBlockedError(cached); err != nil {
				return err
			}
		}
		if p.logger != nil {
			p.logger.Warn("Command Code quota preflight failed; allowing request: %v", fetchErr)
		}
		if p.quotaConfig.FailClosed {
			return p.quotaFetchError(fetchErr)
		}
		return nil
	}

	p.quotaCache.Store(cacheKey, commandCodeQuotaEntry{Snapshot: snapshot, FetchedAt: time.Now()})
	return p.quotaBlockedError(snapshot)
}

func (p *commandCodeProvider) getCachedQuota(cacheKey string) (commandCodeQuotaSnapshot, commandCodeQuotaCacheState) {
	raw, ok := p.quotaCache.Load(cacheKey)
	if !ok {
		return commandCodeQuotaSnapshot{}, commandCodeQuotaCacheMiss
	}
	entry, ok := raw.(commandCodeQuotaEntry)
	if !ok {
		p.quotaCache.Delete(cacheKey)
		return commandCodeQuotaSnapshot{}, commandCodeQuotaCacheMiss
	}
	if time.Since(entry.FetchedAt) > p.quotaConfig.CacheTTL {
		return entry.Snapshot, commandCodeQuotaCacheStale
	}
	return entry.Snapshot, commandCodeQuotaCacheFresh
}

func (p *commandCodeProvider) fetchQuota(apiKey string) (commandCodeQuotaSnapshot, error) {
	credits, err := p.fetchQuotaJSON(apiKey, commandCodeBillingCreditsPath)
	if err != nil {
		return commandCodeQuotaSnapshot{}, err
	}

	usage, usageErr := p.fetchQuotaJSON(apiKey, commandCodeUsageSummaryPath)
	if usageErr != nil && p.logger != nil {
		p.logger.Warn("Command Code usage summary fetch failed; continuing with billing windows only: %v", usageErr)
	}

	snapshot, parseErr := parseCommandCodeQuotaResponse(credits, usage)
	if parseErr != nil {
		return commandCodeQuotaSnapshot{}, parseErr
	}
	return snapshot, nil
}

func (p *commandCodeProvider) fetchQuotaJSON(apiKey string, path string) (map[string]interface{}, error) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.Header.SetMethod(http.MethodGet)
	req.SetRequestURI(p.networkConfig.BaseURL + path)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range p.buildHeaders(apiKey, "") {
		if k == "x-session-id" {
			continue
		}
		req.Header.Set(k, v)
	}

	if p.quotaConfig.DebugLog && p.logger != nil {
		p.logger.Info("Command Code quota preflight request: method=GET url=%s path=%s", p.networkConfig.BaseURL+path, path)
	}
	if err := p.client.DoTimeout(req, resp, p.quotaConfig.Timeout); err != nil {
		return nil, err
	}
	if p.quotaConfig.DebugLog && p.logger != nil {
		p.logger.Info("Command Code quota preflight response: path=%s status=%d body=%s", path, resp.StatusCode(), commandCodeTruncateForLog(string(resp.Body()), commandCodeQuotaLogBodyLimit))
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, fmt.Errorf("%s returned status %d", path, resp.StatusCode())
	}

	var data map[string]interface{}
	if err := sonic.Unmarshal(resp.Body(), &data); err != nil {
		return nil, err
	}
	return data, nil
}

func parseCommandCodeQuotaResponse(credits map[string]interface{}, usage map[string]interface{}) (commandCodeQuotaSnapshot, error) {
	if credits == nil {
		return commandCodeQuotaSnapshot{}, errors.New("credits response missing")
	}

	windows := map[string]commandCodeQuotaWindow{}
	limitReached := commandCodeTruthy(commandCodeMap(credits["windowLimits"])["exceeded"])

	windowLimits := commandCodeMap(credits["windowLimits"])
	fiveHour := parseCommandCodeQuotaWindow(commandCodeMap(windowLimits["fiveHour"]))
	if fiveHour != nil {
		windows[commandCodeQuotaWindowFiveHour] = *fiveHour
		limitReached = limitReached || fiveHour.Exceeded
	}
	weekly := parseCommandCodeQuotaWindow(commandCodeMap(windowLimits["weekly"]))
	if weekly != nil {
		windows[commandCodeQuotaWindowWeekly] = *weekly
		limitReached = limitReached || weekly.Exceeded
	}

	percentUsed := 0.0
	resetAt := time.Time{}
	for _, window := range windows {
		percentUsed = math.Max(percentUsed, window.PercentUsed)
		if window.PercentUsed >= percentUsed && !window.ResetAt.IsZero() {
			resetAt = window.ResetAt
		}
	}

	if usage != nil {
		usedCredits := commandCodeNumber(usage["totalCredits"], usage["totalCost"])
		remainingCredits := commandCodeNumber(commandCodeMap(credits["credits"])["monthlyCredits"])
		if usedCredits > 0 && remainingCredits >= 0 {
			total := usedCredits + remainingCredits
			if total > 0 {
				percentUsed = math.Max(percentUsed, usedCredits/total*100)
			}
		}
	}

	if len(windows) == 0 && percentUsed == 0 {
		return commandCodeQuotaSnapshot{}, errors.New("quota response missing usage windows")
	}

	return commandCodeQuotaSnapshot{
		PercentUsed:  percentUsed,
		ResetAt:      resetAt,
		LimitReached: limitReached || commandCodeTruthy(commandCodeMap(credits["credits"])["belowThreshold"]),
		Windows:      windows,
	}, nil
}

func parseCommandCodeQuotaWindow(data map[string]interface{}) *commandCodeQuotaWindow {
	if data == nil {
		return nil
	}
	used := commandCodeNumber(data["used"])
	capacity := commandCodeNumber(data["cap"], data["limit"])
	percent := 0.0
	if capacity > 0 {
		percent = used / capacity * 100
	}
	return &commandCodeQuotaWindow{
		Used:        used,
		Cap:         capacity,
		PercentUsed: percent,
		ResetAt:     commandCodeUnixMillis(data["resetAt"], data["reset_at"]),
		Exceeded:    commandCodeTruthy(data["exceeded"]),
	}
}

func (p *commandCodeProvider) quotaBlockedError(snapshot commandCodeQuotaSnapshot) *schemas.BifrostError {
	if !snapshot.LimitReached && snapshot.PercentUsed < p.quotaConfig.ThresholdPercent {
		return nil
	}
	message := fmt.Sprintf("Command Code quota preflight blocked request: %.0f%% used", snapshot.PercentUsed)
	if !snapshot.ResetAt.IsZero() {
		message += ", resets at " + snapshot.ResetAt.Format(time.RFC3339)
	}
	return newCommandCodeQuotaError(message, nil)
}

func (p *commandCodeProvider) quotaFetchError(err error) *schemas.BifrostError {
	return newCommandCodeQuotaError("Command Code quota preflight failed", err)
}

func newCommandCodeQuotaError(message string, err error) *schemas.BifrostError {
	statusCode := fasthttp.StatusTooManyRequests
	allowFallbacks := true
	errorType := commandCodeQuotaRateLimitErrorType
	code := commandCodeQuotaRateLimitErrorCode
	return &schemas.BifrostError{
		IsBifrostError: false,
		StatusCode:     &statusCode,
		AllowFallbacks: &allowFallbacks,
		Error: &schemas.ErrorField{
			Type:    &errorType,
			Code:    &code,
			Message: message,
			Error:   err,
		},
	}
}

func (p *commandCodeProvider) quotaCacheKey(apiKey string, key schemas.Key, model string) string {
	keyID := firstNonEmptyCommandCode(key.ID, key.Name, apiKey)
	if len(keyID) > commandCodeQuotaMaxCacheKeyTokenLength {
		keyID = keyID[:commandCodeQuotaMaxCacheKeyTokenLength]
	}
	return keyID + ":" + model
}

func commandCodeMap(value interface{}) map[string]interface{} {
	if result, ok := value.(map[string]interface{}); ok {
		return result
	}
	return nil
}

func commandCodeNumber(values ...interface{}) float64 {
	for _, value := range values {
		switch v := value.(type) {
		case float64:
			return v
		case float32:
			return float64(v)
		case int:
			return float64(v)
		case int64:
			return float64(v)
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err == nil {
				return parsed
			}
		}
	}
	return 0
}

func commandCodeUnixMillis(values ...interface{}) time.Time {
	ms := commandCodeNumber(values...)
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(ms))
}

func commandCodeTruthy(values ...interface{}) bool {
	for _, value := range values {
		switch v := value.(type) {
		case bool:
			return v
		case string:
			normalized := strings.ToLower(strings.TrimSpace(v))
			return normalized == "true" || normalized == "1" || normalized == "yes"
		}
	}
	return false
}

func firstNonEmptyCommandCode(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func commandCodeEnvBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	normalized := strings.ToLower(value)
	return normalized == "1" || normalized == "true" || normalized == "yes" || normalized == "on"
}

func commandCodeEnvFloat(name string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func commandCodeEnvInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func commandCodeTruncateForLog(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "...<truncated>"
}
