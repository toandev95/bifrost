package antigravity

import (
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	schemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// antigravityErrorResponse mirrors Google's standard error envelope.
type antigravityErrorResponse struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// parseAntigravityError converts an upstream HTTP error response into a
// *schemas.BifrostError, overlaying Google's error fields.
func parseAntigravityError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp antigravityErrorResponse
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)

	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	if errorResp.Error.Message != "" {
		bifrostErr.Error.Message = errorResp.Error.Message
	}
	if errorResp.Error.Status != "" {
		status := errorResp.Error.Status
		bifrostErr.Error.Type = &status
	}
	return bifrostErr
}
