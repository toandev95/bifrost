package antigravity

import (
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	schemas "github.com/maximhq/bifrost/core/schemas"
)

// curatedAntigravityModels is the default model catalogue surfaced in the UI.
// Antigravity routes through Google Cloud Code Assist; the exact set available
// depends on the account's plan, but these are the common ids. Users may also
// whitelist any other model id on the key.
var curatedAntigravityModels = []string{
	"gemini-3.1-pro-high",
	"gemini-3.1-pro-low",
	"gemini-3.5-flash-high",
	"gemini-3.5-flash-medium",
	"gemini-3.5-flash-low",
	"claude-sonnet-4-6",
	"claude-opus-4-6-thinking",
	"gpt-oss-120b-medium",
}

// ListModels validates each key (by resolving its token and probing Cloud Code)
// and returns the available model catalogue. A per-key probe failure marks that
// key invalid in the UI; a success marks it valid.
func (p *antigravityProvider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	return providerUtils.HandleMultipleListModelsRequests(ctx, keys, request, p.listModelsByKey)
}

func (p *antigravityProvider) listModelsByKey(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	cred, accessToken, berr := p.resolveToken(ctx, key)
	if berr != nil {
		return nil, berr
	}

	// Probe Cloud Code to validate the credential. loadCodeAssist requires only a
	// valid bearer token and is the lightest authenticated call available.
	if _, _, err := LoadCodeAssist(ctx, accessToken); err != nil {
		p.tokens.invalidate(cred.RefreshToken)
		return nil, providerUtils.NewProviderAPIError("failed to validate Antigravity credential", err, 401, nil, nil)
	}

	// Build the catalogue: curated defaults plus any explicitly whitelisted ids.
	seen := map[string]bool{}
	var data []schemas.Model
	addModel := func(id string) {
		if id == "" || id == "*" || seen[id] {
			return
		}
		seen[id] = true
		data = append(data, schemas.Model{ID: id, OwnedBy: schemas.Ptr(string(schemas.Antigravity))})
	}
	for _, m := range curatedAntigravityModels {
		addModel(m)
	}
	for _, m := range key.Models {
		addModel(m)
	}

	return &schemas.BifrostListModelsResponse{Data: data}, nil
}
