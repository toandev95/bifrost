package antigravity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/providers/gemini"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	schemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

// antigravityProvider implements the schemas.Provider interface for Antigravity
// (Google Cloud Code Assist). It reuses the Gemini converters and wraps the
// request in the Cloud Code "v1internal" envelope, authenticating with a Google
// OAuth access token minted on demand from a stored refresh token.
type antigravityProvider struct {
	logger              schemas.Logger
	client              *fasthttp.Client
	streamingClient     *fasthttp.Client
	networkConfig       schemas.NetworkConfig
	sendBackRawRequest  bool
	sendBackRawResponse bool
	tokens              *tokenManager
	// noProjectHeader caches refresh tokens whose account 403s when the
	// x-goog-user-project header is sent (free-tier managed projects without the
	// Cloud Code API enabled). Once detected, the header is skipped up front.
	noProjectHeader sync.Map
}

// NewAntigravityProvider creates a new Antigravity provider instance.
func NewAntigravityProvider(config *schemas.ProviderConfig, logger schemas.Logger) *antigravityProvider {
	config.CheckAndSetDefaults()

	requestTimeout := time.Second * time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds)
	client := &fasthttp.Client{
		ReadTimeout:         requestTimeout,
		WriteTimeout:        requestTimeout,
		MaxConnsPerHost:     config.NetworkConfig.MaxConnsPerHost,
		MaxIdleConnDuration: 30 * time.Second,
		MaxConnWaitTimeout:  requestTimeout,
		MaxConnDuration:     time.Second * time.Duration(schemas.DefaultMaxConnDurationInSeconds),
		ConnPoolStrategy:    fasthttp.FIFO,
	}

	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client, config.NetworkConfig.AllowPrivateNetwork)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)
	streamingClient := providerUtils.BuildStreamingClient(client)

	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = antigravityBaseURLs[0]
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &antigravityProvider{
		logger:              logger,
		client:              client,
		streamingClient:     streamingClient,
		networkConfig:       config.NetworkConfig,
		sendBackRawRequest:  config.SendBackRawRequest,
		sendBackRawResponse: config.SendBackRawResponse,
		tokens:              newTokenManager(),
	}
}

// GetProviderKey returns the provider identifier.
func (p *antigravityProvider) GetProviderKey() schemas.ModelProvider {
	return schemas.Antigravity
}

func (p *antigravityProvider) streamURL(ctx *schemas.BifrostContext) string {
	return p.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, "/v1internal:streamGenerateContent?alt=sse")
}

// resolveToken parses the credential blob and mints a valid access token.
func (p *antigravityProvider) resolveToken(ctx *schemas.BifrostContext, key schemas.Key) (antigravityCredential, string, *schemas.BifrostError) {
	cred := parseCredential(key.Value.GetValue())
	if cred.RefreshToken == "" {
		return cred, "", providerUtils.NewBifrostOperationError("Antigravity account not connected (missing refresh token); connect via the dashboard", fmt.Errorf("missing refresh token"))
	}
	if cred.ProjectID == "" {
		return cred, "", providerUtils.NewBifrostOperationError("Antigravity account missing Cloud Code project id; reconnect via the dashboard", fmt.Errorf("missing project id"))
	}
	accessToken, err := p.tokens.getAccessToken(ctx, cred.RefreshToken)
	if err != nil {
		return cred, "", providerUtils.NewBifrostOperationError("failed to refresh Antigravity access token", err)
	}
	return cred, accessToken, nil
}

// buildEnvelope converts the Bifrost chat request to a Gemini request, injects
// the native Cloud Code defaults (sessionId, safetySettings, toolConfig,
// generationConfig defaults) into the inner request, and wraps it in the Cloud
// Code envelope so the payload matches what the Antigravity client sends.
func (p *antigravityProvider) buildEnvelope(ctx *schemas.BifrostContext, cred antigravityCredential, request *schemas.BifrostChatRequest) ([]byte, *schemas.BifrostError) {
	geminiReq, err := gemini.ToGeminiChatCompletionRequest(ctx, request)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to convert request to gemini format", err)
	}
	if geminiReq == nil {
		return nil, providerUtils.NewBifrostOperationError("chat completion input is not provided", fmt.Errorf("nil gemini request"))
	}

	geminiJSON, marshalErr := sonic.Marshal(geminiReq)
	if marshalErr != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderRequestMarshal, marshalErr)
	}

	// The model lives in the envelope; strip it (and bifrost-only fallbacks) from
	// the inner request to match the native Cloud Code request shape.
	geminiJSON = deleteJSONField(geminiJSON, "model")
	geminiJSON = deleteJSONField(geminiJSON, "fallbacks")

	// Inject the native inner-request fields the Antigravity client always sends.
	hasTools := providerUtils.JSONFieldExists(geminiJSON, "tools")
	geminiJSON = ensureJSONField(geminiJSON, "safetySettings", defaultSafetySettings())
	if hasTools {
		// Native client forces VALIDATED function-calling mode when tools present.
		geminiJSON = setJSONField(geminiJSON, "toolConfig", map[string]any{
			"functionCallingConfig": map[string]any{"mode": "VALIDATED"},
		})
	}
	geminiJSON = ensureJSONField(geminiJSON, "generationConfig.topK", 40)
	geminiJSON = ensureJSONField(geminiJSON, "generationConfig.topP", 1.0)
	if mx := gjson.GetBytes(geminiJSON, "generationConfig.maxOutputTokens"); mx.Exists() && mx.Int() > maxAntigravityOutputTokens {
		geminiJSON = setJSONField(geminiJSON, "generationConfig.maxOutputTokens", maxAntigravityOutputTokens)
	}
	geminiJSON = setJSONField(geminiJSON, "sessionId", deriveSessionID(cred.Email))

	envelope := requestEnvelope{
		Project:            cred.ProjectID,
		RequestID:          generateRequestID(),
		Request:            geminiJSON,
		Model:              normalizeModelName(request.Model),
		UserAgent:          envelopeUserAgent(cred.Email),
		RequestType:        requestTypeAgent,
		EnabledCreditTypes: []string{"GOOGLE_ONE_AI"},
	}
	body, marshalErr := sonic.Marshal(envelope)
	if marshalErr != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderRequestMarshal, marshalErr)
	}
	return body, nil
}

// buildHeaders returns the native Antigravity (ide profile) request headers for
// a Cloud Code content request. Matching this header set is what makes the
// request indistinguishable from the real client.
func (p *antigravityProvider) buildHeaders(accessToken, projectID string) map[string]string {
	h := map[string]string{
		"User-Agent":         desktopUserAgent(),
		"Accept":             "text/event-stream",
		"Accept-Encoding":    "gzip, deflate, br",
		"x-client-name":      "antigravity",
		"x-client-version":   antigravityVersion,
		"x-vscode-sessionid": vscodeSessionID,
		"Authorization":      "Bearer " + accessToken,
	}
	if projectID != "" {
		h["x-goog-user-project"] = projectID
	}
	return h
}

// JSON helper wrappers (best-effort; preserve input on error).

func deleteJSONField(b []byte, path string) []byte {
	if out, err := providerUtils.DeleteJSONField(b, path); err == nil {
		return out
	}
	return b
}

func setJSONField(b []byte, path string, v any) []byte {
	if out, err := providerUtils.SetJSONField(b, path, v); err == nil {
		return out
	}
	return b
}

func ensureJSONField(b []byte, path string, v any) []byte {
	if providerUtils.JSONFieldExists(b, path) {
		return b
	}
	return setJSONField(b, path, v)
}

// isRetryableStreamError reports whether an SSE read error is a transient
// stream failure (upstream closed the connection early, or the per-chunk idle
// timeout fired) that is worth retrying. Matches both the sentinel errors and
// their message text, since the SSE reader may surface a wrapped/string error.
// Other read errors are left to the normal (non-retried) error path. The actual
// retry is performed by Bifrost's generic retry framework, not here.
func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, providerUtils.ErrStreamClosed) || errors.Is(err, providerUtils.ErrStreamIdleTimeout) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "stream closed") ||
		strings.Contains(msg, "stream idle timeout") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "broken pipe")
}

func (p *antigravityProvider) shouldSkipProjectHeader(refreshToken string) bool {
	_, ok := p.noProjectHeader.Load(refreshToken)
	return ok
}

func (p *antigravityProvider) markSkipProjectHeader(refreshToken string) {
	p.noProjectHeader.Store(refreshToken, true)
}

// sendUnaryOnce performs one non-streaming POST and returns the HTTP status, a
// copy of the body (on 200) or a parsed error (on non-200/network error), plus
// latency and provider headers. projectID == "" omits the x-goog-user-project
// header.
func (p *antigravityProvider) sendUnaryOnce(ctx *schemas.BifrostContext, jsonBody []byte, accessToken, projectID string) (status int, body []byte, latency time.Duration, provHeaders map[string]string, bErr *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.Header.SetMethod(http.MethodPost)
	req.SetRequestURI(p.streamURL(ctx))
	req.Header.SetContentType("application/json")
	providerUtils.SetExtraHeaders(ctx, req, p.networkConfig.ExtraHeaders, nil)
	for k, v := range p.buildHeaders(accessToken, projectID) {
		req.Header.Set(k, v)
	}
	req.SetBody(jsonBody)

	lat, netErr, wait := providerUtils.MakeRequestWithContext(ctx, p.client, req, resp)
	defer wait()
	if netErr != nil {
		return 0, nil, lat, nil, netErr
	}
	provHeaders = providerUtils.ExtractProviderResponseHeaders(resp)
	status = resp.StatusCode()
	if status != fasthttp.StatusOK {
		return status, nil, lat, provHeaders, parseAntigravityError(resp)
	}
	b, decodeErr := providerUtils.CheckAndDecodeBody(resp)
	if decodeErr != nil {
		return status, nil, lat, provHeaders, providerUtils.NewBifrostOperationError("failed to read Antigravity response", decodeErr)
	}
	return status, append([]byte(nil), b...), lat, provHeaders, nil
}

// ChatCompletion performs a logically non-streaming chat completion. Cloud Code
// always streams upstream, so the full SSE body is read and merged into one
// Gemini response, then converted to a Bifrost response.
func (p *antigravityProvider) ChatCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	cred, accessToken, berrr := p.resolveToken(ctx, key)
	if berrr != nil {
		return nil, berrr
	}

	jsonBody, bErr := p.buildEnvelope(ctx, cred, request)
	if bErr != nil {
		return nil, bErr
	}

	sendRawReq := providerUtils.ShouldSendBackRawRequest(ctx, p.sendBackRawRequest)
	sendRawResp := providerUtils.ShouldSendBackRawResponse(ctx, p.sendBackRawResponse)

	includeProject := cred.ProjectID != "" && !p.shouldSkipProjectHeader(cred.RefreshToken)
	projectID := ""
	if includeProject {
		projectID = cred.ProjectID
	}

	status, body, latency, provHeaders, reqErr := p.sendUnaryOnce(ctx, jsonBody, accessToken, projectID)
	// A 403 with the project header set means the managed project lacks the
	// Cloud Code API; retry once without the header (and skip it from now on).
	if status == fasthttp.StatusForbidden && includeProject {
		p.markSkipProjectHeader(cred.RefreshToken)
		status, body, latency, provHeaders, reqErr = p.sendUnaryOnce(ctx, jsonBody, accessToken, "")
	}
	if provHeaders != nil {
		ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, provHeaders)
	}
	if status == fasthttp.StatusUnauthorized {
		p.tokens.invalidate(cred.RefreshToken)
	}
	if reqErr != nil {
		return nil, providerUtils.EnrichError(ctx, reqErr, jsonBody, nil, sendRawReq, sendRawResp)
	}

	merged, aggErr := mergeCloudCodeStream(body)
	if aggErr != nil {
		return nil, providerUtils.EnrichError(ctx, aggErr, jsonBody, body, sendRawReq, sendRawResp)
	}

	response := merged.ToBifrostChatResponse()
	if response.Model == "" {
		response.Model = request.Model
	}
	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.Provider = schemas.Antigravity
	if sendRawReq {
		providerUtils.ParseAndSetRawRequest(&response.ExtraFields, jsonBody)
	}
	if sendRawResp {
		response.ExtraFields.RawResponse = string(body)
	}
	return response, nil
}

// mergeCloudCodeStream parses a full Cloud Code SSE body (chunks wrapped in
// {"response": ...}) and merges them into one Gemini GenerateContentResponse.
func mergeCloudCodeStream(body []byte) (*gemini.GenerateContentResponse, *schemas.BifrostError) {
	merged := &gemini.GenerateContentResponse{}
	parts := []*gemini.Part{}
	var finishReason gemini.FinishReason
	role := "model"

	for _, line := range strings.Split(string(body), "\n") {
		data := extractSSEData(line)
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk streamChunk
		if err := sonic.UnmarshalString(data, &chunk); err != nil || len(chunk.Response) == 0 {
			continue
		}
		var gr gemini.GenerateContentResponse
		if err := sonic.Unmarshal(chunk.Response, &gr); err != nil {
			continue
		}
		if gr.ResponseID != "" {
			merged.ResponseID = gr.ResponseID
		}
		if gr.ModelVersion != "" {
			merged.ModelVersion = gr.ModelVersion
		}
		if gr.UsageMetadata != nil {
			merged.UsageMetadata = gr.UsageMetadata
		}
		if !gr.CreateTime.IsZero() {
			merged.CreateTime = gr.CreateTime
		}
		if len(gr.Candidates) > 0 {
			cand := gr.Candidates[0]
			if cand.FinishReason != "" {
				finishReason = cand.FinishReason
			}
			if cand.Content != nil {
				if cand.Content.Role != "" {
					role = cand.Content.Role
				}
				parts = append(parts, cand.Content.Parts...)
			}
		}
	}

	merged.Candidates = []*gemini.Candidate{
		{
			Index:        0,
			FinishReason: finishReason,
			Content:      &gemini.Content{Role: role, Parts: parts},
		},
	}
	return merged, nil
}

// extractSSEData returns the JSON payload from a single SSE line.
func extractSSEData(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") || strings.HasPrefix(trimmed, "event:") {
		return ""
	}
	if strings.HasPrefix(trimmed, "data:") {
		trimmed = strings.TrimSpace(trimmed[len("data:"):])
	}
	return trimmed
}

// ChatCompletionStream streams a chat completion from Cloud Code, unwrapping the
// {"response": ...} envelope per chunk and converting via the Gemini stream
// converter. When invoked as a Responses-API fallback, chat chunks are
// converted into Responses stream events.
func (p *antigravityProvider) ChatCompletionStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	cred, accessToken, berrr := p.resolveToken(ctx, key)
	if berrr != nil {
		return nil, berrr
	}

	sendRawReq := providerUtils.ShouldSendBackRawRequest(ctx, p.sendBackRawRequest)
	sendRawResp := providerUtils.ShouldSendBackRawResponse(ctx, p.sendBackRawResponse)

	jsonBody, bErr := p.buildEnvelope(ctx, cred, request)
	if bErr != nil {
		return nil, bErr
	}

	providerUtils.SetStreamIdleTimeoutIfEmpty(ctx, p.networkConfig.StreamIdleTimeoutInSeconds)

	includeProject := cred.ProjectID != "" && !p.shouldSkipProjectHeader(cred.RefreshToken)
	projectID := ""
	if includeProject {
		projectID = cred.ProjectID
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.Header.SetMethod(http.MethodPost)
	req.SetRequestURI(p.streamURL(ctx))
	req.Header.SetContentType("application/json")
	providerUtils.SetExtraHeaders(ctx, req, p.networkConfig.ExtraHeaders, nil)
	for k, v := range p.buildHeaders(accessToken, projectID) {
		req.Header.Set(k, v)
	}
	req.SetBody(jsonBody)

	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true
	activeClient := providerUtils.PrepareResponseStreaming(ctx, p.streamingClient, resp)

	startTime := time.Now()
	err := activeClient.Do(req, resp)
	// A 403 with the project header set means the managed project lacks the Cloud
	// Code API; retry once without the header (and skip it from now on).
	if err == nil && resp.StatusCode() == fasthttp.StatusForbidden && includeProject {
		p.markSkipProjectHeader(cred.RefreshToken)
		providerUtils.ReleaseStreamingResponse(ctx, resp)
		req.Header.Del("x-goog-user-project")
		resp = fasthttp.AcquireResponse()
		resp.StreamBody = true
		activeClient = providerUtils.PrepareResponseStreaming(ctx, p.streamingClient, resp)
		err = activeClient.Do(req, resp)
	}
	if err != nil {
		defer providerUtils.ReleaseStreamingResponse(ctx, resp)
		if errors.Is(err, context.Canceled) {
			return nil, providerUtils.EnrichError(ctx, &schemas.BifrostError{
				IsBifrostError: false,
				Error: &schemas.ErrorField{
					Type:    schemas.Ptr(schemas.RequestCancelled),
					Message: schemas.ErrRequestCancelled,
					Error:   err,
				},
			}, jsonBody, nil, sendRawReq, sendRawResp)
		}
		if errors.Is(err, fasthttp.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostTimeoutError(schemas.ErrProviderRequestTimedOut, err), jsonBody, nil, sendRawReq, sendRawResp)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderDoRequest, err), jsonBody, nil, sendRawReq, sendRawResp)
	}

	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	if resp.StatusCode() != fasthttp.StatusOK {
		defer providerUtils.ReleaseStreamingResponse(ctx, resp)
		if resp.StatusCode() == fasthttp.StatusUnauthorized {
			p.tokens.invalidate(cred.RefreshToken)
		}
		providerUtils.MaterializeStreamErrorBody(ctx, resp)
		return nil, providerUtils.EnrichError(ctx, parseAntigravityError(resp), jsonBody, nil, sendRawReq, sendRawResp)
	}

	responseChan := make(chan *schemas.BifrostStreamChunk, schemas.DefaultStreamBufferSize)

	isResponsesFallback := false
	var responsesStreamState *schemas.ChatToResponsesStreamState
	if v, ok := ctx.Value(schemas.BifrostContextKeyIsResponsesToChatCompletionFallback).(bool); ok && v {
		isResponsesFallback = true
		responsesStreamState = schemas.AcquireChatToResponsesStreamState()
	}

	// firstStreamResult lets the goroutine report whether the upstream stream
	// established (delivered at least one read) before any content was emitted.
	// A pre-content transient failure — e.g. the upstream closing the SSE
	// connection early ("stream closed") — is surfaced as a retryable network
	// error RETURNED from this method, so Bifrost's generic retry framework
	// (executeRequestWithRetries + shouldRetry + NetworkConfig.MaxRetries) retries
	// it, instead of the error leaking to the client through the stream channel
	// with no retry. Errors after content has started, or non-transient errors,
	// keep the existing (non-retried) behaviour.
	firstStreamResult := make(chan *schemas.BifrostError, 1)
	var firstSignalOnce sync.Once
	signalFirst := func(e *schemas.BifrostError) { firstSignalOnce.Do(func() { firstStreamResult <- e }) }

	go func() {
		// Safety net: guarantee the caller is unblocked even on an unexpected
		// exit path. Treated as "established" (no retry) since explicit pre-content
		// failures signal a retryable error before returning.
		defer signalFirst(nil)

		defer providerUtils.EnsureStreamFinalizerCalled(ctx, postHookSpanFinalizer)
		defer func() {
			if ctx.Err() == context.Canceled {
				providerUtils.HandleStreamCancellation(ctx, postHookRunner, responseChan, p.logger, postHookSpanFinalizer, jsonBody)
			} else if ctx.Err() == context.DeadlineExceeded {
				providerUtils.HandleStreamTimeout(ctx, postHookRunner, responseChan, p.logger, postHookSpanFinalizer, jsonBody)
			}
			if responsesStreamState != nil {
				schemas.ReleaseChatToResponsesStreamState(responsesStreamState)
			}
			close(responseChan)
		}()
		defer providerUtils.ReleaseStreamingResponse(ctx, resp)

		reader, releaseGzip := providerUtils.DecompressStreamBody(resp)
		defer releaseGzip()

		reader, stopIdleTimeout := providerUtils.NewIdleTimeoutReader(reader, resp.BodyStream(), providerUtils.GetStreamIdleTimeout(ctx), ctx)
		defer stopIdleTimeout()

		stopCancellation := providerUtils.SetupStreamCancellation(ctx, resp.BodyStream(), p.logger)
		defer stopCancellation()

		sseReader := providerUtils.GetSSEDataReader(ctx, reader)
		streamState := gemini.NewGeminiStreamState()

		chunkIndex := 0
		lastChunkTime := startTime
		var responseID, modelName string

		usage := &schemas.BifrostLLMUsage{}
		ctx.SetValue(schemas.BifrostContextKeyStreamAccumulatedUsage, usage)

		var pendingFinalEvent *schemas.BifrostResponsesStreamResponse
		usageSeen := false

		// emitChat forwards one chat chunk, converting to Responses events when in
		// fallback mode. Returns true if the stream must abort.
		emitChat := func(r *schemas.BifrostChatResponse) bool {
			if !isResponsesFallback {
				providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetBifrostResponseForStreamResponse(nil, r, nil, nil, nil, nil), responseChan, postHookSpanFinalizer)
				return false
			}
			if r.Usage != nil {
				usageSeen = true
			}
			for _, ev := range r.ToBifrostResponsesStreamResponse(responsesStreamState) {
				if ev.Type == schemas.ResponsesStreamResponseTypeError {
					bifrostErr := &schemas.BifrostError{
						Type:           schemas.Ptr(string(schemas.ResponsesStreamResponseTypeError)),
						IsBifrostError: false,
						Error:          &schemas.ErrorField{},
					}
					if ev.Message != nil {
						bifrostErr.Error.Message = *ev.Message
					}
					if ev.Param != nil {
						bifrostErr.Error.Param = *ev.Param
					}
					if ev.Code != nil {
						bifrostErr.Error.Code = ev.Code
					}
					ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
					providerUtils.ProcessAndSendBifrostError(ctx, postHookRunner, providerUtils.EnrichError(ctx, bifrostErr, jsonBody, nil, sendRawReq, sendRawResp), responseChan, p.logger, postHookSpanFinalizer)
					return true
				}
				ev.ExtraFields.ChunkIndex = ev.SequenceNumber
				if ev.Type == schemas.ResponsesStreamResponseTypeCompleted || ev.Type == schemas.ResponsesStreamResponseTypeIncomplete {
					pendingFinalEvent = ev
					continue
				}
				ev.ExtraFields.Latency = time.Since(lastChunkTime).Milliseconds()
				lastChunkTime = time.Now()
				providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetBifrostResponseForStreamResponse(nil, nil, ev, nil, nil, nil), responseChan, postHookSpanFinalizer)
			}
			return false
		}

		// emittedContent becomes true once a real content chunk has been sent to
		// the client. Until then, a transient stream failure (or stall) is treated
		// as a pre-content failure and surfaced to the generic retry framework —
		// keepalive/empty SSE lines do NOT count as content, so an upstream that
		// sends a keepalive then closes early is still retried.
		emittedContent := false
		for {
			if ctx.Err() != nil {
				return
			}
			data, readErr := sseReader.ReadDataLine()
			if readErr != nil {
				if ctx.Err() != nil {
					return
				}
				if readErr.Error() == "EOF" {
					signalFirst(nil)
					break
				}
				// Pre-content transient stream failure (upstream closed the SSE
				// connection / idle timeout before any chunk was emitted): surface
				// it to Bifrost's generic retry framework as a retryable network
				// error instead of emitting an un-retried error chunk to the client.
				// IsBifrostError must be false so the generic shouldRetry classifier
				// does not treat it as a terminal internal error; Message =
				// ErrProviderNetworkError makes it retryable there.
				if !emittedContent && isRetryableStreamError(readErr) {
					retryErr := &schemas.BifrostError{
						IsBifrostError: false,
						Error: &schemas.ErrorField{
							Message: schemas.ErrProviderNetworkError,
							Error:   readErr,
						},
					}
					signalFirst(providerUtils.EnrichError(ctx, retryErr, jsonBody, nil, sendRawReq, sendRawResp))
					return
				}
				signalFirst(nil)
				ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
				p.logger.Warn("Error reading Antigravity stream: %v", readErr)
				providerUtils.ProcessAndSendError(ctx, postHookRunner, readErr, responseChan, p.logger, postHookSpanFinalizer)
				return
			}
			jsonData := strings.TrimSpace(string(data))
			if jsonData == "" || jsonData == "[DONE]" {
				continue
			}

			var chunk streamChunk
			if err := sonic.UnmarshalString(jsonData, &chunk); err != nil || len(chunk.Response) == 0 {
				continue
			}
			var gr gemini.GenerateContentResponse
			if err := sonic.Unmarshal(chunk.Response, &gr); err != nil {
				continue
			}

			if gr.ResponseID != "" && responseID == "" {
				responseID = gr.ResponseID
			}
			if gr.ModelVersion != "" && modelName == "" {
				modelName = gr.ModelVersion
			}

			response, convErr, isLast := gr.ToBifrostChatCompletionStream(streamState)
			if convErr != nil {
				ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendBifrostError(ctx, postHookRunner, providerUtils.EnrichError(ctx, convErr, jsonBody, nil, sendRawReq, sendRawResp), responseChan, p.logger, postHookSpanFinalizer)
				return
			}
			if response == nil {
				continue
			}

			if responseID != "" {
				response.ID = responseID
			}
			if modelName != "" {
				response.Model = modelName
			} else if response.Model == "" {
				response.Model = request.Model
			}
			if response.Usage != nil {
				*usage = *response.Usage
			}
			response.ExtraFields = schemas.BifrostResponseExtraFields{
				ChunkIndex: chunkIndex,
				Latency:    time.Since(lastChunkTime).Milliseconds(),
			}
			if sendRawResp {
				response.ExtraFields.RawResponse = jsonData
			}
			lastChunkTime = time.Now()
			chunkIndex++

			// First real content chunk: the stream is genuinely producing output,
			// so mark it established (unblocks the caller; disables pre-content retry).
			if !emittedContent {
				emittedContent = true
				signalFirst(nil)
			}

			if isLast {
				if sendRawReq {
					providerUtils.ParseAndSetRawRequest(&response.ExtraFields, jsonBody)
				}
				response.ExtraFields.Latency = time.Since(startTime).Milliseconds()
				if emitChat(response) {
					return
				}
				break
			}
			if emitChat(response) {
				return
			}
		}

		if isResponsesFallback {
			if pendingFinalEvent != nil {
				if usageSeen && pendingFinalEvent.Response != nil {
					pendingFinalEvent.Response.Usage = usage.ToResponsesResponseUsage()
				}
				if sendRawReq {
					providerUtils.ParseAndSetRawRequest(&pendingFinalEvent.ExtraFields, jsonBody)
				}
				pendingFinalEvent.ExtraFields.Latency = time.Since(startTime).Milliseconds()
				ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetBifrostResponseForStreamResponse(nil, nil, pendingFinalEvent, nil, nil, nil), responseChan, postHookSpanFinalizer)
			}
			return
		}

		ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	}()

	// Block until the goroutine reports the stream established (or a pre-content
	// transient failure). A retryable error here is returned so the generic retry
	// framework can re-attempt; otherwise the live stream channel is returned.
	if ferr := <-firstStreamResult; ferr != nil {
		return nil, ferr
	}
	return responseChan, nil
}

// Responses maps to ChatCompletion (Antigravity has no native Responses API).
func (p *antigravityProvider) Responses(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	chatResponse, err := p.ChatCompletion(ctx, key, request.ToChatRequest())
	if err != nil {
		return nil, err
	}
	return chatResponse.ToBifrostResponsesResponse(), nil
}

// ResponsesStream maps to ChatCompletionStream with Responses-event conversion.
func (p *antigravityProvider) ResponsesStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	ctx.SetValue(schemas.BifrostContextKeyIsResponsesToChatCompletionFallback, true)
	return p.ChatCompletionStream(ctx, postHookRunner, postHookSpanFinalizer, key, request.ToChatRequest())
}
