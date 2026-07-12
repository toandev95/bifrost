package commandcode

import (
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	schemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// parseCommandCodeError converts an upstream HTTP error response into a *schemas.BifrostError.
// It relies on the shared HTTP status mapping and overlays Command Code's error fields.
func parseCommandCodeError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp commandCodeErrorResponse
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)

	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}

	switch {
	case errorResp.Error != nil && errorResp.Error.Message != "":
		bifrostErr.Error.Message = errorResp.Error.Message
		if errorResp.Error.Type != "" {
			bifrostErr.Error.Type = &errorResp.Error.Type
		}
		if errorResp.Error.Code != "" {
			bifrostErr.Error.Code = &errorResp.Error.Code
		}
	case errorResp.Message != "":
		bifrostErr.Error.Message = errorResp.Message
	}

	return bifrostErr
}
