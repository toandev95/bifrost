package commandcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/providers/openai"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	schemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// commandCodeProvider implements the schemas.Provider interface for Command Code.
type commandCodeProvider struct {
	logger              schemas.Logger
	client              *fasthttp.Client
	streamingClient     *fasthttp.Client
	networkConfig       schemas.NetworkConfig
	sendBackRawRequest  bool
	sendBackRawResponse bool
	quotaCache          sync.Map
	quotaConfig         commandCodeQuotaConfig
}

// NewCommandCodeProvider creates a new Command Code provider instance.
func NewCommandCodeProvider(config *schemas.ProviderConfig, logger schemas.Logger) *commandCodeProvider {
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
		config.NetworkConfig.BaseURL = commandCodeDefaultBaseURL
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &commandCodeProvider{
		logger:              logger,
		client:              client,
		streamingClient:     streamingClient,
		networkConfig:       config.NetworkConfig,
		sendBackRawRequest:  config.SendBackRawRequest,
		sendBackRawResponse: config.SendBackRawResponse,
		quotaConfig:         newCommandCodeQuotaConfig(),
	}
}

// GetProviderKey returns the provider identifier.
func (p *commandCodeProvider) GetProviderKey() schemas.ModelProvider {
	return schemas.CommandCode
}

// buildHeaders builds the Command Code request headers including auth.
func (p *commandCodeProvider) buildHeaders(apiKey, sessionID string) map[string]string {
	return map[string]string{
		"Authorization":          "Bearer " + apiKey,
		"x-command-code-version": commandCodeVersion,
		"x-cli-environment":      "production",
		"x-project-slug":         commandCodeProjectSlug(),
		"x-taste-learning":       "false",
		"x-co-flag":              "false",
		"x-session-id":           sessionID,
		"User-Agent":             "cli",
	}
}

func (p *commandCodeProvider) chatURL(ctx *schemas.BifrostContext) string {
	return p.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, commandCodeChatPath)
}

// responses performs a non-streaming Responses request by aggregating Command Code's
// upstream SSE protocol.
func (p *commandCodeProvider) responses(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	if validationErr := validateCommandCodeResponsesRequest(request); validationErr != nil {
		return nil, validationErr
	}
	chatRequest := request.ToChatRequest()
	apiKey := key.Value.GetValue()
	if apiKey == "" {
		return nil, providerUtils.NewBifrostOperationError("Command Code API key required", fmt.Errorf("missing api key"))
	}
	if quotaErr := p.preflightQuota(apiKey, key, chatRequest.Model); quotaErr != nil {
		return nil, quotaErr
	}

	sessionID := uuid.NewString()
	jsonBody, marshalErr := sonic.Marshal(buildCommandCodeRequest(chatRequest, sessionID))
	if marshalErr != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderRequestMarshal, marshalErr)
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.Header.SetMethod(http.MethodPost)
	req.SetRequestURI(p.chatURL(ctx))
	req.Header.SetContentType("application/json")
	providerUtils.SetExtraHeaders(ctx, req, p.networkConfig.ExtraHeaders, nil)
	for k, v := range p.buildHeaders(apiKey, sessionID) {
		req.Header.Set(k, v)
	}
	req.SetBody(jsonBody)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, p.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonBody, nil, providerUtils.ShouldSendBackRawRequest(ctx, p.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, p.sendBackRawResponse))
	}

	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.EnrichError(ctx, parseCommandCodeError(resp), jsonBody, nil, providerUtils.ShouldSendBackRawRequest(ctx, p.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, p.sendBackRawResponse))
	}

	body, decodeErr := providerUtils.CheckAndDecodeBody(resp)
	if decodeErr != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to read Command Code response", decodeErr)
	}

	response, aggErr := aggregateCommandCodeStream(body, chatRequest.Model)
	if aggErr != nil {
		return nil, providerUtils.EnrichError(ctx, aggErr, jsonBody, body, providerUtils.ShouldSendBackRawRequest(ctx, p.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, p.sendBackRawResponse))
	}
	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.Provider = schemas.CommandCode
	if providerUtils.ShouldSendBackRawRequest(ctx, p.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&response.ExtraFields, jsonBody)
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, p.sendBackRawResponse) {
		response.ExtraFields.RawResponse = string(body)
	}
	return commandCodeResponsesResponse(response), nil
}

// validateCommandCodeResponsesRequest rejects Responses features that the upstream
// protocol cannot represent without silently losing state or tool definitions.
func validateCommandCodeResponsesRequest(request *schemas.BifrostResponsesRequest) *schemas.BifrostError {
	if request == nil {
		return newCommandCodeRequestError("Command Code request is required", errors.New("missing request"))
	}
	if request.Params == nil {
		return nil
	}
	if request.Params.PreviousResponseID != nil && strings.TrimSpace(*request.Params.PreviousResponseID) != "" {
		return newCommandCodeRequestError(
			"Command Code does not support previous_response_id; resend the complete conversation in input",
			errors.New("previous_response_id is not supported"),
		)
	}
	for _, tool := range request.Params.Tools {
		if tool.Type != schemas.ResponsesToolTypeFunction || tool.ResponsesToolFunction == nil || tool.Name == nil || strings.TrimSpace(*tool.Name) == "" {
			return newCommandCodeRequestError(
				"Command Code supports Responses function tools only",
				errors.New("unsupported Responses tool"),
			)
		}
	}
	if choice := request.Params.ToolChoice; choice != nil {
		if choice.ResponsesToolChoiceStr == nil || (*choice.ResponsesToolChoiceStr != "" && *choice.ResponsesToolChoiceStr != string(schemas.ResponsesToolChoiceTypeAuto)) {
			return newCommandCodeRequestError(
				"Command Code supports only the default or auto Responses tool_choice",
				errors.New("unsupported Responses tool_choice"),
			)
		}
	}
	return nil
}

func newCommandCodeRequestError(message string, err error) *schemas.BifrostError {
	bifrostErr := providerUtils.NewBifrostOperationError(message, err)
	statusCode := fasthttp.StatusBadRequest
	bifrostErr.StatusCode = &statusCode
	return bifrostErr
}

func commandCodeResponsesResponse(chatResponse *schemas.BifrostChatResponse) *schemas.BifrostResponsesResponse {
	response := chatResponse.ToBifrostResponsesResponse()
	completedAt := int(time.Now().Unix())
	response.CompletedAt = &completedAt
	return response
}

// aggregateCommandCodeStream parses a full Command Code SSE body into a single chat response.
func aggregateCommandCodeStream(body []byte, model string) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls []schemas.ChatAssistantMessageToolCall
	finishReason := string(schemas.BifrostFinishReasonStop)
	var usage *schemas.BifrostLLMUsage

	for _, line := range strings.Split(string(body), "\n") {
		data := parseSSELine(line)
		if data == "" || data == "[DONE]" {
			continue
		}
		var event commandCodeStreamEvent
		if err := sonic.UnmarshalString(data, &event); err != nil {
			continue
		}
		switch event.Type {
		case "text-delta":
			content.WriteString(event.Text)
		case "reasoning-delta":
			reasoning.WriteString(event.Text)
		case "tool-call":
			toolCalls = append(toolCalls, eventToToolCall(&event, len(toolCalls)))
		case "finish":
			finishReason = mapFinishReason(event.FinishReason)
			usage = usageToBifrost(event.TotalUsage)
		case "error":
			msg := "Command Code stream error"
			if event.Error != nil && event.Error.Message != "" {
				msg = event.Error.Message
			}
			return nil, providerUtils.NewBifrostOperationError(msg, errors.New(msg))
		}
	}

	contentStr := content.String()
	message := &schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleAssistant,
		Content: &schemas.ChatMessageContent{ContentStr: &contentStr},
	}
	if reasoning.Len() > 0 || len(toolCalls) > 0 {
		assistant := &schemas.ChatAssistantMessage{}
		if reasoning.Len() > 0 {
			r := reasoning.String()
			assistant.Reasoning = &r
		}
		if len(toolCalls) > 0 {
			assistant.ToolCalls = toolCalls
		}
		message.ChatAssistantMessage = assistant
	}

	fr := finishReason
	return &schemas.BifrostChatResponse{
		ID:      "resp_" + uuid.NewString(),
		Object:  "chat.completion",
		Model:   model,
		Created: int(time.Now().Unix()),
		Choices: []schemas.BifrostResponseChoice{
			{
				Index:        0,
				FinishReason: &fr,
				ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
					Message: message,
				},
			},
		},
		Usage: usage,
	}, nil
}

// eventToToolCall converts a tool-call stream event into a Bifrost tool call.
func eventToToolCall(event *commandCodeStreamEvent, index int) schemas.ChatAssistantMessageToolCall {
	id := event.ToolCallID
	if id == "" {
		id = event.ID
	}
	if id == "" {
		id = uuid.NewString()
	}
	name := event.ToolName
	if name == "" {
		name = event.Name
	}
	callType := "function"
	args := event.toolCallArguments()
	return schemas.ChatAssistantMessageToolCall{
		Index:    uint16(index),
		Type:     &callType,
		ID:       &id,
		Function: schemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: args},
	}
}

// parseSSELine extracts the JSON payload from a single SSE line.
func parseSSELine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") || strings.HasPrefix(trimmed, "event:") {
		return ""
	}
	if strings.HasPrefix(trimmed, "data:") {
		trimmed = strings.TrimSpace(trimmed[len("data:"):])
	}
	return trimmed
}

// responsesStream translates Command Code's agent SSE protocol into Responses events.
func (p *commandCodeProvider) responsesStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	if validationErr := validateCommandCodeResponsesRequest(request); validationErr != nil {
		return nil, validationErr
	}
	chatRequest := request.ToChatRequest()
	apiKey := key.Value.GetValue()
	if apiKey == "" {
		return nil, providerUtils.NewBifrostOperationError("Command Code API key required", fmt.Errorf("missing api key"))
	}
	if quotaErr := p.preflightQuota(apiKey, key, chatRequest.Model); quotaErr != nil {
		return nil, quotaErr
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, p.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, p.sendBackRawResponse)

	sessionID := uuid.NewString()
	jsonBody, marshalErr := sonic.Marshal(buildCommandCodeRequest(chatRequest, sessionID))
	if marshalErr != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderRequestMarshal, marshalErr)
	}

	providerUtils.SetStreamIdleTimeoutIfEmpty(ctx, p.networkConfig.StreamIdleTimeoutInSeconds)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true
	defer fasthttp.ReleaseRequest(req)

	req.Header.SetMethod(http.MethodPost)
	req.SetRequestURI(p.chatURL(ctx))
	req.Header.SetContentType("application/json")
	req.Header.Set("Accept", "text/event-stream")
	providerUtils.SetExtraHeaders(ctx, req, p.networkConfig.ExtraHeaders, nil)
	for k, v := range p.buildHeaders(apiKey, sessionID) {
		req.Header.Set(k, v)
	}
	req.SetBody(jsonBody)

	activeClient := providerUtils.PrepareResponseStreaming(ctx, p.streamingClient, resp)

	startTime := time.Now()
	err := activeClient.Do(req, resp)
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
			}, jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
		}
		if errors.Is(err, fasthttp.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostTimeoutError(schemas.ErrProviderRequestTimedOut, err), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderDoRequest, err), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
	}

	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	if resp.StatusCode() != fasthttp.StatusOK {
		defer providerUtils.ReleaseStreamingResponse(ctx, resp)
		providerUtils.MaterializeStreamErrorBody(ctx, resp)
		return nil, providerUtils.EnrichError(ctx, parseCommandCodeError(resp), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
	}

	responseChan := make(chan *schemas.BifrostStreamChunk, schemas.DefaultStreamBufferSize)

	// Command Code speaks an agent/chat-shaped upstream protocol. Convert its
	// synthesized chunks directly to Responses events; this provider exposes no
	// Chat Completions operation.
	responsesStreamState := schemas.AcquireChatToResponsesStreamState()

	go func() {
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

		reader, drained := providerUtils.DrainNonSSEStreamReader(resp, reader)
		if drained {
			ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
			providerUtils.ProcessAndSendError(ctx, postHookRunner, errors.New("Command Code returned non-SSE response for streaming request"), responseChan, p.logger, postHookSpanFinalizer)
			return
		}

		sseReader := providerUtils.GetSSEDataReader(ctx, reader)

		chunkIndex := 0
		toolCallIndex := 0
		messageID := "resp_" + uuid.NewString()
		modelName := chatRequest.Model
		finishReason := string(schemas.BifrostFinishReasonStop)
		sentRole := false
		lastChunkTime := startTime

		usage := &schemas.BifrostLLMUsage{}
		ctx.SetValue(schemas.BifrostContextKeyStreamAccumulatedUsage, usage)

		// pendingFinalEvent holds the responses "completed"/"incomplete" event so we
		// can attach usage before sending it as the very last event.
		var pendingFinalEvent *schemas.BifrostResponsesStreamResponse
		usageSeen := false

		// emitChat converts an upstream delta into OpenAI Responses stream events.
		// Command Code only exposes the Responses API, so this stays local to this
		// provider instead of widening the shared Chat-to-Responses fallback behavior.
		emitChat := func(resp *schemas.BifrostChatResponse) bool {
			if resp.Usage != nil {
				usageSeen = true
			}
			for _, ev := range resp.ToBifrostResponsesStreamResponse(responsesStreamState) {
				normalizeCommandCodeResponsesStreamEvent(ev, messageID, modelName, int(startTime.Unix()))
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
					providerUtils.ProcessAndSendBifrostError(ctx, postHookRunner, providerUtils.EnrichError(ctx, bifrostErr, jsonBody, nil, sendBackRawRequest, sendBackRawResponse), responseChan, p.logger, postHookSpanFinalizer)
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

		for {
			if ctx.Err() != nil {
				return
			}
			data, readErr := sseReader.ReadDataLine()
			if readErr != nil {
				if ctx.Err() != nil {
					return
				}
				if readErr != io.EOF {
					ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
					p.logger.Warn("Error reading Command Code stream: %v", readErr)
					providerUtils.ProcessAndSendError(ctx, postHookRunner, readErr, responseChan, p.logger, postHookSpanFinalizer)
					return
				}
				break
			}
			jsonData := strings.TrimSpace(string(data))
			if jsonData == "" || jsonData == "[DONE]" {
				continue
			}

			var event commandCodeStreamEvent
			if err := sonic.UnmarshalString(jsonData, &event); err != nil {
				p.logger.Warn("Failed to parse Command Code stream event: %v", err)
				continue
			}

			if event.Type == "error" {
				msg := "Command Code stream error"
				if event.Error != nil && event.Error.Message != "" {
					msg = event.Error.Message
				}
				ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendBifrostError(ctx, postHookRunner, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(msg, errors.New(msg)), jsonBody, nil, sendBackRawRequest, sendBackRawResponse), responseChan, p.logger, postHookSpanFinalizer)
				return
			}

			if event.Type == "finish" {
				finishReason = mapFinishReason(event.FinishReason)
				if u := usageToBifrost(event.TotalUsage); u != nil {
					*usage = *u
				}
				break
			}

			delta := eventToStreamDelta(&event, &toolCallIndex, &sentRole)
			if delta == nil {
				continue
			}

			response := &schemas.BifrostChatResponse{
				ID:      messageID,
				Object:  "chat.completion.chunk",
				Model:   modelName,
				Created: int(startTime.Unix()),
				Choices: []schemas.BifrostResponseChoice{
					{
						Index:                    0,
						ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: delta},
					},
				},
				ExtraFields: schemas.BifrostResponseExtraFields{
					ChunkIndex: chunkIndex,
					Latency:    time.Since(lastChunkTime).Milliseconds(),
				},
			}
			if sendBackRawResponse {
				response.ExtraFields.RawResponse = jsonData
			}
			lastChunkTime = time.Now()
			chunkIndex++
			if emitChat(response) {
				return
			}
		}

		finalResponse := providerUtils.CreateBifrostChatCompletionChunkResponse(messageID, usage, &finishReason, chunkIndex, modelName, int(startTime.Unix()))

		// Feed the synthesized terminal chunk through the converter so it emits
		// the closing output events and response.completed/response.incomplete.
		if emitChat(finalResponse) {
			return
		}
		if pendingFinalEvent != nil {
			if usageSeen && pendingFinalEvent.Response != nil {
				pendingFinalEvent.Response.Usage = usage.ToResponsesResponseUsage()
			}
			if sendBackRawRequest {
				providerUtils.ParseAndSetRawRequest(&pendingFinalEvent.ExtraFields, jsonBody)
			}
			pendingFinalEvent.ExtraFields.Latency = time.Since(startTime).Milliseconds()
			ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
			providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetBifrostResponseForStreamResponse(nil, nil, pendingFinalEvent, nil, nil, nil), responseChan, postHookSpanFinalizer)
		}
	}()

	return responseChan, nil
}

// normalizeCommandCodeResponsesStreamEvent fills fields omitted by the generic
// Chat-to-Responses converter. Those fields are required on Response lifecycle
// payloads by OpenAI-compatible SDKs.
func normalizeCommandCodeResponsesStreamEvent(event *schemas.BifrostResponsesStreamResponse, responseID, model string, createdAt int) {
	if event == nil || event.Response == nil {
		return
	}

	response := event.Response
	response.ID = schemas.Ptr(responseID)
	response.Object = "response"
	response.Model = model
	response.CreatedAt = createdAt

	switch event.Type {
	case schemas.ResponsesStreamResponseTypeCreated, schemas.ResponsesStreamResponseTypeInProgress:
		response.Status = schemas.Ptr("in_progress")
	case schemas.ResponsesStreamResponseTypeCompleted:
		response.Status = schemas.Ptr("completed")
		completedAt := int(time.Now().Unix())
		response.CompletedAt = &completedAt
	case schemas.ResponsesStreamResponseTypeIncomplete:
		response.Status = schemas.Ptr("incomplete")
		completedAt := int(time.Now().Unix())
		response.CompletedAt = &completedAt
	}
}

// eventToStreamDelta converts a non-finish/non-error stream event into a delta.
// Returns nil for events that produce no client-visible delta.
func eventToStreamDelta(event *commandCodeStreamEvent, toolCallIndex *int, sentRole *bool) *schemas.ChatStreamResponseChoiceDelta {
	delta := &schemas.ChatStreamResponseChoiceDelta{}
	emit := false

	switch event.Type {
	case "text-delta":
		if event.Text != "" {
			text := event.Text
			delta.Content = &text
			emit = true
		}
	case "reasoning-delta":
		// The shared fallback converter cannot represent Command Code's raw
		// reasoning deltas as a complete OpenAI reasoning item. Suppressing them
		// avoids emitting an invalid empty output_text item before a tool call.
		return nil
	case "tool-call":
		toolCall := eventToToolCall(event, *toolCallIndex)
		*toolCallIndex++
		delta.ToolCalls = []schemas.ChatAssistantMessageToolCall{toolCall}
		emit = true
	default:
		// reasoning-end and unknown events produce no delta.
	}

	if emit && !*sentRole {
		role := string(schemas.ChatMessageRoleAssistant)
		delta.Role = &role
		*sentRole = true
	}

	if !emit {
		return nil
	}
	return delta
}

func (p *commandCodeProvider) Responses(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	return p.responses(ctx, key, request)
}

func (p *commandCodeProvider) ResponsesStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return p.responsesStream(ctx, postHookRunner, postHookSpanFinalizer, key, request)
}

func (p *commandCodeProvider) ChatCompletion(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ChatCompletionRequest, p.GetProviderKey())
}

func (p *commandCodeProvider) ChatCompletionStream(_ *schemas.BifrostContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ChatCompletionStreamRequest, p.GetProviderKey())
}

// ListModels lists Command Code models and validates each key. Command Code's
// /provider/v1/models endpoint is OpenAI-style and requires only Bearer auth, so we
// reuse the shared OpenAI list-models handler — this also populates per-key
// KeyStatuses, which drives the valid/invalid indicator in the UI.
func (p *commandCodeProvider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	return openai.HandleOpenAIListModelsRequest(
		ctx,
		p.client,
		request,
		p.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, commandCodeModelsPath),
		keys,
		p.networkConfig.ExtraHeaders,
		p.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, p.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, p.sendBackRawResponse),
	)
}

// TextCompletion is not supported by Command Code.
func (p *commandCodeProvider) TextCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TextCompletionRequest, p.GetProviderKey())
}

// TextCompletionStream is not supported by Command Code.
func (p *commandCodeProvider) TextCompletionStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostTextCompletionRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TextCompletionStreamRequest, p.GetProviderKey())
}

// CountTokens is not supported by Command Code.
func (p *commandCodeProvider) CountTokens(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CountTokensRequest, p.GetProviderKey())
}

// Compaction is not supported by Command Code.
func (p *commandCodeProvider) Compaction(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostCompactionRequest) (*schemas.BifrostCompactionResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CompactionRequest, p.GetProviderKey())
}

// Embedding is not supported by Command Code.
func (p *commandCodeProvider) Embedding(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.EmbeddingRequest, p.GetProviderKey())
}

// Rerank is not supported by Command Code.
func (p *commandCodeProvider) Rerank(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostRerankRequest) (*schemas.BifrostRerankResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.RerankRequest, p.GetProviderKey())
}

// OCR is not supported by Command Code.
func (p *commandCodeProvider) OCR(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostOCRRequest) (*schemas.BifrostOCRResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.OCRRequest, p.GetProviderKey())
}

// Speech is not supported by Command Code.
func (p *commandCodeProvider) Speech(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechRequest, p.GetProviderKey())
}

// SpeechStream is not supported by Command Code.
func (p *commandCodeProvider) SpeechStream(_ *schemas.BifrostContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.BifrostSpeechRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechStreamRequest, p.GetProviderKey())
}

// Transcription is not supported by Command Code.
func (p *commandCodeProvider) Transcription(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionRequest, p.GetProviderKey())
}

// TranscriptionStream is not supported by Command Code.
func (p *commandCodeProvider) TranscriptionStream(_ *schemas.BifrostContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.BifrostTranscriptionRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionStreamRequest, p.GetProviderKey())
}

// ImageGeneration is not supported by Command Code.
func (p *commandCodeProvider) ImageGeneration(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationRequest, p.GetProviderKey())
}

// ImageGenerationStream is not supported by Command Code.
func (p *commandCodeProvider) ImageGenerationStream(_ *schemas.BifrostContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.BifrostImageGenerationRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationStreamRequest, p.GetProviderKey())
}

// ImageEdit is not supported by Command Code.
func (p *commandCodeProvider) ImageEdit(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditRequest, p.GetProviderKey())
}

// ImageEditStream is not supported by Command Code.
func (p *commandCodeProvider) ImageEditStream(_ *schemas.BifrostContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.BifrostImageEditRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditStreamRequest, p.GetProviderKey())
}

// ImageVariation is not supported by Command Code.
func (p *commandCodeProvider) ImageVariation(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostImageVariationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageVariationRequest, p.GetProviderKey())
}

// VideoGeneration is not supported by Command Code.
func (p *commandCodeProvider) VideoGeneration(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoGenerationRequest, p.GetProviderKey())
}

// VideoRetrieve is not supported by Command Code.
func (p *commandCodeProvider) VideoRetrieve(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRetrieveRequest, p.GetProviderKey())
}

// VideoDownload is not supported by Command Code.
func (p *commandCodeProvider) VideoDownload(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDownloadRequest, p.GetProviderKey())
}

// VideoDelete is not supported by Command Code.
func (p *commandCodeProvider) VideoDelete(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostVideoDeleteRequest) (*schemas.BifrostVideoDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDeleteRequest, p.GetProviderKey())
}

// VideoList is not supported by Command Code.
func (p *commandCodeProvider) VideoList(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostVideoListRequest) (*schemas.BifrostVideoListResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoListRequest, p.GetProviderKey())
}

// VideoRemix is not supported by Command Code.
func (p *commandCodeProvider) VideoRemix(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostVideoRemixRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRemixRequest, p.GetProviderKey())
}

// BatchCreate is not supported by Command Code.
func (p *commandCodeProvider) BatchCreate(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostBatchCreateRequest) (*schemas.BifrostBatchCreateResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCreateRequest, p.GetProviderKey())
}

// BatchList is not supported by Command Code.
func (p *commandCodeProvider) BatchList(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchListRequest) (*schemas.BifrostBatchListResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchListRequest, p.GetProviderKey())
}

// BatchRetrieve is not supported by Command Code.
func (p *commandCodeProvider) BatchRetrieve(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchRetrieveRequest) (*schemas.BifrostBatchRetrieveResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchRetrieveRequest, p.GetProviderKey())
}

// BatchCancel is not supported by Command Code.
func (p *commandCodeProvider) BatchCancel(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchCancelRequest) (*schemas.BifrostBatchCancelResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCancelRequest, p.GetProviderKey())
}

// BatchDelete is not supported by Command Code.
func (p *commandCodeProvider) BatchDelete(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchDeleteRequest) (*schemas.BifrostBatchDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchDeleteRequest, p.GetProviderKey())
}

// BatchResults is not supported by Command Code.
func (p *commandCodeProvider) BatchResults(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchResultsRequest) (*schemas.BifrostBatchResultsResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchResultsRequest, p.GetProviderKey())
}

// FileUpload is not supported by Command Code.
func (p *commandCodeProvider) FileUpload(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostFileUploadRequest) (*schemas.BifrostFileUploadResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileUploadRequest, p.GetProviderKey())
}

// FileList is not supported by Command Code.
func (p *commandCodeProvider) FileList(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostFileListRequest) (*schemas.BifrostFileListResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileListRequest, p.GetProviderKey())
}

// FileRetrieve is not supported by Command Code.
func (p *commandCodeProvider) FileRetrieve(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostFileRetrieveRequest) (*schemas.BifrostFileRetrieveResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileRetrieveRequest, p.GetProviderKey())
}

// FileDelete is not supported by Command Code.
func (p *commandCodeProvider) FileDelete(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostFileDeleteRequest) (*schemas.BifrostFileDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileDeleteRequest, p.GetProviderKey())
}

// FileContent is not supported by Command Code.
func (p *commandCodeProvider) FileContent(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostFileContentRequest) (*schemas.BifrostFileContentResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileContentRequest, p.GetProviderKey())
}

// CachedContentCreate is not supported by Command Code.
func (p *commandCodeProvider) CachedContentCreate(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostCachedContentCreateRequest) (*schemas.BifrostCachedContentCreateResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CachedContentCreateRequest, p.GetProviderKey())
}

// CachedContentList is not supported by Command Code.
func (p *commandCodeProvider) CachedContentList(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostCachedContentListRequest) (*schemas.BifrostCachedContentListResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CachedContentListRequest, p.GetProviderKey())
}

// CachedContentRetrieve is not supported by Command Code.
func (p *commandCodeProvider) CachedContentRetrieve(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostCachedContentRetrieveRequest) (*schemas.BifrostCachedContentRetrieveResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CachedContentRetrieveRequest, p.GetProviderKey())
}

// CachedContentUpdate is not supported by Command Code.
func (p *commandCodeProvider) CachedContentUpdate(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostCachedContentUpdateRequest) (*schemas.BifrostCachedContentUpdateResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CachedContentUpdateRequest, p.GetProviderKey())
}

// CachedContentDelete is not supported by Command Code.
func (p *commandCodeProvider) CachedContentDelete(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostCachedContentDeleteRequest) (*schemas.BifrostCachedContentDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CachedContentDeleteRequest, p.GetProviderKey())
}

// ContainerCreate is not supported by Command Code.
func (p *commandCodeProvider) ContainerCreate(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostContainerCreateRequest) (*schemas.BifrostContainerCreateResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerCreateRequest, p.GetProviderKey())
}

// ContainerList is not supported by Command Code.
func (p *commandCodeProvider) ContainerList(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerListRequest) (*schemas.BifrostContainerListResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerListRequest, p.GetProviderKey())
}

// ContainerRetrieve is not supported by Command Code.
func (p *commandCodeProvider) ContainerRetrieve(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerRetrieveRequest) (*schemas.BifrostContainerRetrieveResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerRetrieveRequest, p.GetProviderKey())
}

// ContainerDelete is not supported by Command Code.
func (p *commandCodeProvider) ContainerDelete(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerDeleteRequest) (*schemas.BifrostContainerDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerDeleteRequest, p.GetProviderKey())
}

// ContainerFileCreate is not supported by Command Code.
func (p *commandCodeProvider) ContainerFileCreate(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostContainerFileCreateRequest) (*schemas.BifrostContainerFileCreateResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileCreateRequest, p.GetProviderKey())
}

// ContainerFileList is not supported by Command Code.
func (p *commandCodeProvider) ContainerFileList(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerFileListRequest) (*schemas.BifrostContainerFileListResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileListRequest, p.GetProviderKey())
}

// ContainerFileRetrieve is not supported by Command Code.
func (p *commandCodeProvider) ContainerFileRetrieve(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerFileRetrieveRequest) (*schemas.BifrostContainerFileRetrieveResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileRetrieveRequest, p.GetProviderKey())
}

// ContainerFileContent is not supported by Command Code.
func (p *commandCodeProvider) ContainerFileContent(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerFileContentRequest) (*schemas.BifrostContainerFileContentResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileContentRequest, p.GetProviderKey())
}

// ContainerFileDelete is not supported by Command Code.
func (p *commandCodeProvider) ContainerFileDelete(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerFileDeleteRequest) (*schemas.BifrostContainerFileDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileDeleteRequest, p.GetProviderKey())
}

// Passthrough is not supported by Command Code.
func (p *commandCodeProvider) Passthrough(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughRequest, p.GetProviderKey())
}

// PassthroughStream is not supported by Command Code.
func (p *commandCodeProvider) PassthroughStream(_ *schemas.BifrostContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.BifrostPassthroughRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughStreamRequest, p.GetProviderKey())
}
