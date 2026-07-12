package lib

import (
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/kvstore"
)

const (
	responsesStatePrefix   = "responses-state:"
	responsesStateTTL      = 24 * time.Hour
	responsesStateMaxBytes = 8 << 20
)

type responsesConversationState struct {
	Input []schemas.ResponsesMessage `json:"input"`
}

// ResponsesStateStore emulates previous_response_id for providers that do not
// retain Responses conversations themselves.
type ResponsesStateStore struct {
	store *kvstore.Store
}

func NewResponsesStateStore(store *kvstore.Store) *ResponsesStateStore {
	return &ResponsesStateStore{store: store}
}

func (s *ResponsesStateStore) Expand(req *schemas.BifrostResponsesRequest) error {
	if req == nil || req.Params == nil || req.Params.PreviousResponseID == nil || strings.TrimSpace(*req.Params.PreviousResponseID) == "" {
		return nil
	}
	if s == nil || s.store == nil {
		return fmt.Errorf("previous_response_id is not available because Responses conversation state is disabled")
	}
	previousID := strings.TrimSpace(*req.Params.PreviousResponseID)
	value, err := s.store.Get(responsesStatePrefix + previousID)
	if err != nil {
		return fmt.Errorf("previous_response_id %q is unknown or expired; resend the complete conversation in input", previousID)
	}
	state, ok := value.(*responsesConversationState)
	if !ok || state == nil {
		return fmt.Errorf("previous_response_id %q is invalid; resend the complete conversation in input", previousID)
	}
	input := append(append(make([]schemas.ResponsesMessage, 0, len(state.Input)+len(req.Input)), state.Input...), req.Input...)
	if _, err := sonic.Marshal(input); err != nil {
		return fmt.Errorf("failed to expand previous_response_id: %w", err)
	}
	req.Input = input
	req.Params.PreviousResponseID = nil
	return nil
}

func (s *ResponsesStateStore) Save(req *schemas.BifrostResponsesRequest, response *schemas.BifrostResponsesResponse) {
	if s == nil || s.store == nil || req == nil || response == nil || response.ID == nil || strings.TrimSpace(*response.ID) == "" {
		return
	}
	input := append(append(make([]schemas.ResponsesMessage, 0, len(req.Input)+len(response.Output)), req.Input...), response.Output...)
	encoded, err := sonic.Marshal(input)
	if err != nil || len(encoded) > responsesStateMaxBytes {
		return
	}
	_ = s.store.SetWithTTL(responsesStatePrefix+*response.ID, &responsesConversationState{Input: input}, responsesStateTTL)
}
