package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"rss-studio/internal/observability"
)

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type apiErrorResponse struct {
	Error     apiErrorBody `json:"error"`
	RequestID string       `json:"request_id"`
}

type apiInputError struct {
	Code    string
	Message string
}

func isAPIRequest(request *http.Request) bool {
	return request.URL.Path == "/api/v1" || strings.HasPrefix(request.URL.Path, "/api/v1/")
}

func parseAPICredentials(request *http.Request) (authCredentials, *apiInputError) {
	if !isJSONRequest(request) {
		return authCredentials{}, &apiInputError{
			Code:    "unsupported_media_type",
			Message: "Request Content-Type must be application/json.",
		}
	}

	var credentials authCredentials
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credentials); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return authCredentials{}, &apiInputError{
				Code:    "unknown_field",
				Message: "Request body contains an unknown field.",
			}
		}
		return authCredentials{}, &apiInputError{
			Code:    "invalid_json",
			Message: "Request body must contain valid JSON.",
		}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return authCredentials{}, &apiInputError{
			Code:    "invalid_json",
			Message: "Request body must contain one JSON object.",
		}
	}
	return credentials, nil
}

func writeAPIJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeAPIError(writer http.ResponseWriter, request *http.Request, status int, code, message string) {
	writeAPIJSON(writer, status, apiErrorResponse{
		Error:     apiErrorBody{Code: code, Message: message},
		RequestID: observability.RequestID(request),
	})
}
