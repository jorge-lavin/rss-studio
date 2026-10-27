package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"rss-studio/internal/application"
	"rss-studio/internal/domain"
	"rss-studio/internal/observability"
	"rss-studio/internal/policy"
	"rss-studio/internal/version"
)

const (
	protocolVersion = "2025-03-26"
	maxRequestBody  = 1 << 20
)

var supportedProtocolVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
	"2025-11-25": true,
}

// MCPAuthenticator supplies service-token identity for an MCP request.
type MCPAuthenticator interface {
	IdentityFromRequest(*http.Request) (policy.Identity, error)
}

type Handler struct {
	service       *application.Service
	authenticator MCPAuthenticator
	now           func() time.Time
}

// NewHandler returns a stateless Streamable HTTP MCP handler.
func NewHandler(service *application.Service, authenticator MCPAuthenticator) http.Handler {
	return NewHandlerWithClock(service, authenticator, time.Now)
}

// NewHandlerWithClock is equivalent to NewHandler with an injectable clock
// for deterministic shared rate-limit tests.
func NewHandlerWithClock(service *application.Service, authenticator MCPAuthenticator, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{service: service, authenticator: authenticator, now: now}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
}

type capabilities struct {
	Tools toolCapability `json:"tools"`
}

type toolCapability struct {
	ListChanged bool `json:"listChanged"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []tool `json:"tools"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type previewInput struct {
	SourceURL string        `json:"source_url"`
	Outputs   []outputInput `json:"outputs"`
}

type outputInput struct {
	Name     string                `json:"name"`
	Operator domain.FilterOperator `json:"operator"`
	Keyword  string                `json:"keyword"`
	Match    domain.FilterMatch    `json:"match"`
	Filters  []filterInput         `json:"filters"`
}

type filterInput struct {
	Operator domain.FilterOperator `json:"operator"`
	Keyword  string                `json:"keyword"`
}

type createFeedInput struct {
	Name           string                `json:"name"`
	SourceURL      string                `json:"source_url"`
	Operator       domain.FilterOperator `json:"operator"`
	Keyword        string                `json:"keyword"`
	Match          domain.FilterMatch    `json:"match"`
	Filters        []filterInput         `json:"filters"`
	IdempotencyKey string                `json:"idempotency_key"`
}

type deleteFeedInput struct {
	Path           string `json:"path"`
	IdempotencyKey string `json:"idempotency_key"`
}

type splitFeedInput struct {
	SourceURL      string        `json:"source_url"`
	Outputs        []outputInput `json:"outputs"`
	IdempotencyKey string        `json:"idempotency_key"`
}

type feedOutput struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	URL       string    `json:"url"`
	SourceURL string    `json:"source_url"`
	Operator  string    `json:"operator"`
	Keyword   string    `json:"keyword"`
	Match     string    `json:"match"`
	Filters   []filterOutput `json:"filters"`
	CreatedAt time.Time `json:"created_at"`
}

type previewOutput struct {
	Name         string           `json:"name"`
	Operator     string           `json:"operator"`
	Keyword      string           `json:"keyword"`
	Match        string           `json:"match"`
	Filters      []filterOutput   `json:"filters"`
	Count        int              `json:"count"`
	SampleTitles []string         `json:"sample_titles"`
	FeedImageURL string           `json:"feed_image_url,omitempty"`
	Episodes     []previewEpisode `json:"episodes"`
}

type filterOutput struct {
	Operator string `json:"operator"`
	Keyword  string `json:"keyword"`
}

type previewEpisode struct {
	Title    string `json:"title"`
	ImageURL string `json:"image_url,omitempty"`
}

var tools = []tool{
	{
		Name:        "list_feeds",
		Description: "List authenticated user's published filtered RSS feeds.",
		InputSchema: objectSchema(nil, []string{}),
	},
	{
		Name:        "preview_feed",
		Description: "Preview matching episodes and artwork without publishing a feed.",
		InputSchema: previewSchema(),
	},
	{
		Name:        "create_feed",
		Description: "Publish one filtered RSS feed for the authenticated user.",
		InputSchema: objectSchema(map[string]any{
			"name":            map[string]any{"type": "string"},
			"source_url":      map[string]any{"type": "string"},
			"operator":        map[string]any{"type": "string", "enum": operators(), "default": string(domain.FilterContains)},
			"keyword":         map[string]any{"type": "string"},
			"match":           map[string]any{"type": "string", "enum": filterMatches(), "default": string(domain.FilterAny)},
			"filters":         filtersSchema(),
			"idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 255},
		}, []string{"name", "source_url"}),
	},
	{
		Name:        "create_split_feeds",
		Description: "Publish multiple filtered RSS feeds from one source feed.",
		InputSchema: splitSchema(),
	},
	{
		Name:        "delete_feed",
		Description: "Delete one of the authenticated user's published feeds by its path.",
		InputSchema: objectSchema(map[string]any{
			"path":            map[string]any{"type": "string", "description": "Public feed path, such as /feeds/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.xml."},
			"idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 255},
		}, []string{"path"}),
	},
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "MCP endpoint accepts POST requests only.", http.StatusMethodNotAllowed)
		return
	}
	if h.authenticator == nil {
		unauthorized(writer)
		return
	}
	if !hasBearerAuthorization(request) {
		unauthorized(writer)
		return
	}
	identity, identityErr := h.authenticator.IdentityFromRequest(request)
	if identityErr != nil || !identity.ServiceToken {
		unauthorized(writer)
		return
	}
	owner := identity.User
	request = request.WithContext(policy.WithIdentity(request.Context(), identity))
	if version := request.Header.Get("MCP-Protocol-Version"); version != "" && !supportedProtocolVersions[version] {
		writeHTTPRPCError(writer, http.StatusBadRequest, nil, -32600, "Unsupported MCP protocol version.")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	var message rpcRequest
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&message); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeHTTPRPCError(writer, http.StatusBadRequest, nil, -32700, "Parse error.")
		return
	}
	if message.JSONRPC != "2.0" || message.Method == "" {
		writeHTTPRPCError(writer, http.StatusBadRequest, responseID(message.ID), -32600, "Invalid Request.")
		return
	}

	if isNotification(message) {
		if message.Method == "notifications/initialized" {
			writer.WriteHeader(http.StatusAccepted)
			return
		}
		writer.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := h.handleMessage(request.Context(), request, owner, message)
	if rpcErr != nil {
		writeRPCResponse(writer, message.ID, nil, rpcErr)
		return
	}
	writeRPCResponse(writer, message.ID, result, nil)
}

func (h *Handler) handleMessage(ctx context.Context, request *http.Request, owner domain.User, message rpcRequest) (any, *rpcError) {
	switch message.Method {
	case "initialize":
		var input initializeParams
		if err := decodeParams(message.Params, &input); err != nil {
			return nil, invalidParams()
		}
		selectedVersion := protocolVersion
		if supportedProtocolVersions[input.ProtocolVersion] {
			selectedVersion = input.ProtocolVersion
		}
		return initializeResult{
			ProtocolVersion: selectedVersion,
			Capabilities:    capabilities{Tools: toolCapability{}},
			ServerInfo:      serverInfo{Name: "rss-studio", Version: version.Version},
		}, nil
	case "notifications/initialized":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return toolsListResult{Tools: tools}, nil
	case "tools/call":
		return h.callTool(ctx, request, owner, message.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found."}
	}
}

func (h *Handler) callTool(ctx context.Context, request *http.Request, owner domain.User, raw json.RawMessage) (any, *rpcError) {
	var input toolCallParams
	if err := decodeToolCallParams(raw, &input); err != nil || input.Name == "" {
		return nil, invalidParams()
	}
	identity, ok := policy.IdentityFromContext(ctx)
	if !ok || !identity.ServiceToken {
		return nil, authenticationError()
	}
	requiredScope, operation, known := toolPolicy(input.Name)
	if !known {
		return nil, invalidParamsMessage("Unknown tool.")
	}
	policyStarted := time.Now()
	if err := policy.Authorize(identity, requiredScope); err != nil {
		observability.Record(ctx, observability.Event{Event: observability.EventPolicy, Operation: operation, Result: "denied", DurationMS: time.Since(policyStarted).Milliseconds()})
		return nil, insufficientScopeError()
	}
	observability.Record(ctx, observability.Event{Event: observability.EventPolicy, Operation: operation, Result: "allowed", DurationMS: time.Since(policyStarted).Milliseconds()})
	if result := h.checkRateLimit(ctx, identity, operation); result != nil {
		return result, nil
	}

	switch input.Name {
	case "list_feeds":
		var args struct{}
		if err := decodeArguments(input.Arguments, &args); err != nil {
			return toolError("Invalid tool arguments."), nil
		}
		feeds, err := h.service.ListFeeds(owner)
		if err != nil {
			return toolError("Could not list feeds."), nil
		}
		result := make([]feedOutput, len(feeds))
		for i, feed := range feeds {
			result[i] = feedOutputFor(request, feed)
		}
		return jsonToolResult(result), nil

	case "preview_feed":
		var args previewInput
		if err := decodeArguments(input.Arguments, &args); err != nil {
			return toolError("Invalid tool arguments."), nil
		}
		previews, err := h.service.PreviewForOwner(ctx, owner, args.SourceURL, previewRules(args.Outputs))
		if err != nil {
			return toolError(serviceErrorMessage(err)), nil
		}
		result := make([]previewOutput, len(previews))
		for i, preview := range previews {
			episodes := make([]previewEpisode, len(preview.Episodes))
			for j, episode := range preview.Episodes {
				episodes[j] = previewEpisode{Title: episode.Title, ImageURL: episode.ImageURL}
			}
			result[i] = previewOutput{
				Name: preview.Name, Operator: string(preview.Operator), Keyword: preview.Keyword, Match: string(preview.Match), Filters: filterOutputs(preview.Filters),
				Count: preview.Count, SampleTitles: preview.Titles, FeedImageURL: preview.FeedImageURL,
				Episodes: episodes,
			}
		}
		return jsonToolResult(result), nil

	case "create_feed":
		var args createFeedInput
		if err := decodeArguments(input.Arguments, &args); err != nil {
			return toolError("Invalid tool arguments."), nil
		}
		if !validCreateFeedInput(args) {
			return toolError("Invalid tool arguments."), nil
		}
		requestHash, err := application.IdempotencyRequestHash("create_feed", struct {
			Name      string                `json:"name"`
			SourceURL string                `json:"source_url"`
			Operator  domain.FilterOperator `json:"operator"`
			Keyword   string                `json:"keyword"`
			Match     domain.FilterMatch    `json:"match"`
			Filters   []filterInput         `json:"filters"`
		}{args.Name, args.SourceURL, args.Operator, args.Keyword, args.Match, args.Filters})
		if err != nil {
			return toolError("Could not create feed."), nil
		}
		result, err := h.service.ExecuteIdempotent(ctx, owner, identity.CredentialHash, args.IdempotencyKey, requestHash, func() (any, error) {
			feed, err := h.service.CreateFeedWithFilters(ctx, owner, args.Name, args.SourceURL, args.Match, filterConditions(args.Operator, args.Keyword, args.Filters))
			if err != nil {
				return nil, err
			}
			return feedOutputFor(request, feed), nil
		})
		if err != nil {
			return toolError(serviceErrorMessage(err)), nil
		}
		return jsonToolResultBytes(result), nil

	case "create_split_feeds":
		var args splitFeedInput
		if err := decodeArguments(input.Arguments, &args); err != nil {
			return toolError("Invalid tool arguments."), nil
		}
		if !validSplitFeedInput(args) {
			return toolError("Invalid tool arguments."), nil
		}
		requestHash, err := application.IdempotencyRequestHash("split_feeds", struct {
			SourceURL string        `json:"source_url"`
			Outputs   []outputInput `json:"outputs"`
		}{args.SourceURL, args.Outputs})
		if err != nil {
			return toolError("Could not publish feeds."), nil
		}
		result, err := h.service.ExecuteIdempotent(ctx, owner, identity.CredentialHash, args.IdempotencyKey, requestHash, func() (any, error) {
			feeds, err := h.service.CreateSplitFeeds(ctx, owner, args.SourceURL, previewRules(args.Outputs))
			if err != nil {
				return nil, err
			}
			output := make([]feedOutput, len(feeds))
			for i, feed := range feeds {
				output[i] = feedOutputFor(request, feed)
			}
			return output, nil
		})
		if err != nil {
			return toolError(serviceErrorMessage(err)), nil
		}
		return jsonToolResultBytes(result), nil

	case "delete_feed":
		var args deleteFeedInput
		if err := decodeArguments(input.Arguments, &args); err != nil {
			return toolError("Invalid tool arguments."), nil
		}
		if !validDeleteFeedInput(args) {
			return toolError("Invalid tool arguments."), nil
		}
		path := feedPath(args.Path)
		requestHash, err := application.IdempotencyRequestHash("delete_feed", struct {
			Path string `json:"path"`
		}{path})
		if err != nil {
			return toolError("Could not delete feed."), nil
		}
		result, err := h.service.ExecuteIdempotent(ctx, owner, identity.CredentialHash, args.IdempotencyKey, requestHash, func() (any, error) {
			if err := h.service.DeleteFeedContext(ctx, owner, path); err != nil {
				return nil, err
			}
			return map[string]any{"deleted": true, "path": path}, nil
		})
		if err != nil {
			return toolError(serviceErrorMessage(err)), nil
		}
		return jsonToolResultBytes(result), nil
	}
	return nil, invalidParamsMessage("Unknown tool.")
}

func (h *Handler) checkRateLimit(ctx context.Context, identity policy.Identity, operation string) *toolResult {
	if h.service == nil {
		return nil
	}
	store := h.service.RateLimitStore()
	spec, known := policy.RateLimitSpecFor(operation)
	if !known || store == nil {
		return nil
	}
	decision, err := store.CheckRateLimits(spec.Operation, spec.Limit, spec.Window, policy.RateLimitBuckets(identity), h.now().UTC())
	if err != nil {
		observability.Record(ctx, observability.Event{Event: observability.EventRateLimit, Operation: operation, Result: "unavailable"})
		result := toolError("Rate limiting is temporarily unavailable.")
		return &result
	}
	if !decision.Allowed {
		observability.Record(ctx, observability.Event{Event: observability.EventRateLimit, Operation: operation, Result: "rejected", RateRejected: true})
		result := toolError("Rate limit exceeded. Try again later.")
		return &result
	}
	return nil
}

func toolPolicy(name string) (scope, operation string, ok bool) {
	switch name {
	case "list_feeds":
		return domain.ScopeFeedsRead, policy.OperationFeedsList, true
	case "preview_feed":
		return domain.ScopeFeedsPreview, policy.OperationFeedsPreview, true
	case "create_feed":
		return domain.ScopeFeedsWrite, policy.OperationFeedsCreate, true
	case "create_split_feeds":
		return domain.ScopeFeedsWrite, policy.OperationFeedsSplit, true
	case "delete_feed":
		return domain.ScopeFeedsDelete, policy.OperationFeedsDelete, true
	default:
		return "", "", false
	}
}

func validCreateFeedInput(input createFeedInput) bool {
	return input.Name != "" && input.SourceURL != "" && (input.Keyword != "" || len(input.Filters) > 0) && validIdempotencyKey(input.IdempotencyKey)
}

func validSplitFeedInput(input splitFeedInput) bool {
	return input.SourceURL != "" && len(input.Outputs) > 0 && validIdempotencyKey(input.IdempotencyKey)
}

func validDeleteFeedInput(input deleteFeedInput) bool {
	return strings.TrimSpace(input.Path) != "" && validIdempotencyKey(input.IdempotencyKey)
}

func validIdempotencyKey(value string) bool {
	trimmed := strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if trimmed == "" || len(value) > 255 {
		return false
	}
	for _, character := range trimmed {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func previewRules(outputs []outputInput) []application.PreviewRule {
	rules := make([]application.PreviewRule, len(outputs))
	for i, output := range outputs {
		rules[i] = application.PreviewRule{Name: output.Name, Operator: output.Operator, Keyword: output.Keyword, Match: output.Match, Filters: filterConditions(output.Operator, output.Keyword, output.Filters)}
	}
	return rules
}

func filterConditions(operator domain.FilterOperator, keyword string, filters []filterInput) []domain.FilterCondition {
	if len(filters) == 0 {
		return []domain.FilterCondition{{Operator: operator, Keyword: keyword}}
	}
	conditions := make([]domain.FilterCondition, len(filters))
	for i, filter := range filters {
		conditions[i] = domain.FilterCondition{Operator: filter.Operator, Keyword: filter.Keyword}
	}
	return conditions
}

func filterOutputs(filters []domain.FilterCondition) []filterOutput {
	output := make([]filterOutput, len(filters))
	for i, filter := range filters {
		output[i] = filterOutput{Operator: string(filter.Operator), Keyword: filter.Keyword}
	}
	return output
}

func feedOutputFor(request *http.Request, feed domain.Feed) feedOutput {
	path := "/feeds/" + feed.Path + ".xml"
	return feedOutput{
		Name: feed.Name, Path: path, URL: baseURL(request) + path,
		SourceURL: feed.SourceURL, Operator: string(feed.Operator), Keyword: feed.Keyword,
		Match: string(feed.Match), Filters: filterOutputs(feed.Filters),
		CreatedAt: feed.CreatedAt,
	}
}

func feedPath(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "/feeds/")
	return strings.TrimSuffix(value, ".xml")
}

func serviceErrorMessage(err error) string {
	switch {
	case errors.Is(err, application.ErrQuotaExceeded):
		return "Quota exceeded."
	case errors.Is(err, application.ErrInvalidFeed):
		return err.Error()
	case errors.Is(err, application.ErrFeedNotFound):
		return "Feed not found."
	case errors.Is(err, application.ErrSourceUnavailable):
		return "Source feed could not be fetched or processed."
	case errors.Is(err, application.ErrIdempotencyConflict):
		return "Idempotency key was already used with a different request."
	case errors.Is(err, application.ErrIdempotencyUnavailable):
		return "Idempotency protection is temporarily unavailable."
	case errors.Is(err, application.ErrQuotaUnavailable):
		return "Quotas are temporarily unavailable."
	default:
		return "Could not complete feed operation."
	}
}

func decodeParams(raw json.RawMessage, value any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, value)
}

func decodeArguments(raw json.RawMessage, value any) error {
	arguments := strings.TrimSpace(string(raw))
	if arguments == "" {
		arguments = "{}"
	}
	if arguments == "null" {
		return errors.New("tool arguments must be an object")
	}
	if arguments[0] != '{' {
		return errors.New("tool arguments must be an object")
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("tool arguments must contain one JSON object")
	}
	var fields any
	if err := json.Unmarshal([]byte(arguments), &fields); err != nil {
		return err
	}
	if containsJSONNull(fields) {
		return errors.New("tool arguments cannot contain null fields")
	}
	return nil
}

func containsJSONNull(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case []any:
		for _, item := range value {
			if containsJSONNull(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range value {
			if containsJSONNull(item) {
				return true
			}
		}
	}
	return false
}

func decodeToolCallParams(raw json.RawMessage, value any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("tool call parameters are required")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("tool call parameters must contain one JSON object")
	}
	return nil
}

func jsonToolResult(value any) toolResult {
	data, err := json.Marshal(value)
	if err != nil {
		return toolError("Could not encode tool result.")
	}
	return toolResult{Content: []toolContent{{Type: "text", Text: string(data)}}}
}

func jsonToolResultBytes(data []byte) toolResult {
	if len(data) == 0 || !json.Valid(data) {
		return toolError("Could not encode tool result.")
	}
	return toolResult{Content: []toolContent{{Type: "text", Text: string(data)}}}
}

func toolError(message string) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: message}}, IsError: true}
}

func invalidParams() *rpcError {
	return &rpcError{Code: -32602, Message: "Invalid params."}
}

func invalidParamsMessage(message string) *rpcError {
	return &rpcError{Code: -32602, Message: message}
}

func insufficientScopeError() *rpcError {
	return &rpcError{Code: -32003, Message: "Insufficient scope."}
}

func authenticationError() *rpcError {
	return &rpcError{Code: -32001, Message: "Authentication required."}
}

func isNotification(message rpcRequest) bool {
	return len(message.ID) == 0
}

func responseID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func writeRPCResponse(writer http.ResponseWriter, id json.RawMessage, result any, rpcErr *rpcError) {
	writer.Header().Set("Content-Type", "application/json")
	response := rpcResponse{JSONRPC: "2.0", ID: responseID(id), Result: result, Error: rpcErr}
	_ = json.NewEncoder(writer).Encode(response)
}

func writeHTTPRPCError(writer http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	response := rpcResponse{JSONRPC: "2.0", ID: responseID(id), Error: &rpcError{Code: code, Message: message}}
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(response)
}

func unauthorized(writer http.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="rss-studio"`)
	writeHTTPRPCError(writer, http.StatusUnauthorized, nil, -32001, "Authentication required.")
}

func hasBearerAuthorization(request *http.Request) bool {
	parts := strings.Fields(request.Header.Get("Authorization"))
	return len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && parts[1] != ""
}

func baseURL(request *http.Request) string {
	scheme := request.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
		if request.TLS != nil {
			scheme = "https"
		}
	}
	return scheme + "://" + request.Host
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func previewSchema() map[string]any {
	return objectSchema(map[string]any{
		"source_url": map[string]any{"type": "string"},
		"outputs": map[string]any{
			"type": "array", "minItems": 1, "maxItems": application.MaxSplitOutputs,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":     map[string]any{"type": "string"},
					"operator": map[string]any{"type": "string", "enum": operators(), "default": string(domain.FilterContains)},
					"keyword":  map[string]any{"type": "string"},
					"match":    map[string]any{"type": "string", "enum": filterMatches(), "default": string(domain.FilterAny)},
					"filters":  filtersSchema(),
				},
				"required":             []string{"name"},
				"additionalProperties": false,
			},
		},
	}, []string{"source_url", "outputs"})
}

func splitSchema() map[string]any {
	schema := previewSchema()
	properties := schema["properties"].(map[string]any)
	properties["idempotency_key"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 255}
	schema["required"] = []string{"source_url", "outputs"}
	return schema
}

func operators() []string {
	return []string{string(domain.FilterContains), string(domain.FilterStartsWith), string(domain.FilterEndsWith), string(domain.FilterEquals)}
}

func filterMatches() []string {
	return []string{string(domain.FilterAny), string(domain.FilterAll)}
}

func filtersSchema() map[string]any {
	return map[string]any{
		"type": "array", "minItems": 1, "maxItems": application.MaxFilterConditions,
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operator": map[string]any{"type": "string", "enum": operators(), "default": string(domain.FilterContains)},
				"keyword":  map[string]any{"type": "string"},
			},
			"required": []string{"keyword"}, "additionalProperties": false,
		},
	}
}
