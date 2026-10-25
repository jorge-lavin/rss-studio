package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/domain"
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

// UserAuthenticator supplies the authenticated tenant for an MCP request.
type UserAuthenticator interface {
	UserFromRequest(*http.Request) (domain.User, error)
}

type Handler struct {
	service       *application.Service
	authenticator UserAuthenticator
}

// NewHandler returns a stateless Streamable HTTP MCP handler.
func NewHandler(service *application.Service, authenticator UserAuthenticator) http.Handler {
	return &Handler{service: service, authenticator: authenticator}
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
}

type createFeedInput struct {
	Name      string                `json:"name"`
	SourceURL string                `json:"source_url"`
	Operator  domain.FilterOperator `json:"operator"`
	Keyword   string                `json:"keyword"`
}

type deleteFeedInput struct {
	Path string `json:"path"`
}

type feedOutput struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	URL       string    `json:"url"`
	SourceURL string    `json:"source_url"`
	Operator  string    `json:"operator"`
	Keyword   string    `json:"keyword"`
	CreatedAt time.Time `json:"created_at"`
}

type previewOutput struct {
	Name         string   `json:"name"`
	Operator     string   `json:"operator"`
	Keyword      string   `json:"keyword"`
	Count        int      `json:"count"`
	SampleTitles []string `json:"sample_titles"`
}

var tools = []tool{
	{
		Name:        "list_feeds",
		Description: "List authenticated user's published filtered RSS feeds.",
		InputSchema: objectSchema(nil, []string{}),
	},
	{
		Name:        "preview_feed",
		Description: "Preview matching episode titles without publishing a feed.",
		InputSchema: previewSchema(),
	},
	{
		Name:        "create_feed",
		Description: "Publish one filtered RSS feed for the authenticated user.",
		InputSchema: objectSchema(map[string]any{
			"name":       map[string]any{"type": "string"},
			"source_url": map[string]any{"type": "string"},
			"operator":   map[string]any{"type": "string", "enum": operators()},
			"keyword":    map[string]any{"type": "string"},
		}, []string{"name", "source_url", "keyword"}),
	},
	{
		Name:        "create_split_feeds",
		Description: "Publish multiple filtered RSS feeds from one source feed.",
		InputSchema: previewSchema(),
	},
	{
		Name:        "delete_feed",
		Description: "Delete one of the authenticated user's published feeds by its path.",
		InputSchema: objectSchema(map[string]any{
			"path": map[string]any{"type": "string", "description": "Public feed path, such as /feeds/my-feed.xml."},
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
	owner, err := h.authenticator.UserFromRequest(request)
	if err != nil {
		unauthorized(writer)
		return
	}
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
	if err := decodeParams(raw, &input); err != nil || input.Name == "" {
		return nil, invalidParams()
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
		previews, err := h.service.Preview(ctx, args.SourceURL, previewRules(args.Outputs))
		if err != nil {
			return toolError(serviceErrorMessage(err)), nil
		}
		result := make([]previewOutput, len(previews))
		for i, preview := range previews {
			result[i] = previewOutput{
				Name: preview.Name, Operator: string(preview.Operator), Keyword: preview.Keyword,
				Count: preview.Count, SampleTitles: preview.Titles,
			}
		}
		return jsonToolResult(result), nil

	case "create_feed":
		var args createFeedInput
		if err := decodeArguments(input.Arguments, &args); err != nil {
			return toolError("Invalid tool arguments."), nil
		}
		feed, err := h.service.CreateFeedWithOperator(ctx, owner, args.Name, args.SourceURL, args.Operator, args.Keyword)
		if err != nil {
			return toolError(serviceErrorMessage(err)), nil
		}
		return jsonToolResult(feedOutputFor(request, feed)), nil

	case "create_split_feeds":
		var args previewInput
		if err := decodeArguments(input.Arguments, &args); err != nil {
			return toolError("Invalid tool arguments."), nil
		}
		feeds, err := h.service.CreateSplitFeeds(ctx, owner, args.SourceURL, previewRules(args.Outputs))
		if err != nil {
			return toolError(serviceErrorMessage(err)), nil
		}
		result := make([]feedOutput, len(feeds))
		for i, feed := range feeds {
			result[i] = feedOutputFor(request, feed)
		}
		return jsonToolResult(result), nil

	case "delete_feed":
		var args deleteFeedInput
		if err := decodeArguments(input.Arguments, &args); err != nil {
			return toolError("Invalid tool arguments."), nil
		}
		path := feedPath(args.Path)
		if err := h.service.DeleteFeed(owner, path); err != nil {
			return toolError(serviceErrorMessage(err)), nil
		}
		return jsonToolResult(map[string]any{"deleted": true, "path": path}), nil
	default:
		return toolError("Unknown tool."), nil
	}
}

func previewRules(outputs []outputInput) []application.PreviewRule {
	rules := make([]application.PreviewRule, len(outputs))
	for i, output := range outputs {
		rules[i] = application.PreviewRule{Name: output.Name, Operator: output.Operator, Keyword: output.Keyword}
	}
	return rules
}

func feedOutputFor(request *http.Request, feed domain.Feed) feedOutput {
	path := "/feeds/" + feed.Path + ".xml"
	return feedOutput{
		Name: feed.Name, Path: path, URL: baseURL(request) + path,
		SourceURL: feed.SourceURL, Operator: string(feed.Operator), Keyword: feed.Keyword,
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
	case errors.Is(err, application.ErrInvalidFeed):
		return err.Error()
	case errors.Is(err, application.ErrFeedNotFound):
		return "Feed not found."
	case errors.Is(err, application.ErrSourceUnavailable):
		return "Source feed could not be fetched or processed."
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
	if arguments == "" || arguments == "null" {
		arguments = "{}"
	}
	if arguments[0] != '{' {
		return errors.New("tool arguments must be an object")
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func jsonToolResult(value any) toolResult {
	data, err := json.Marshal(value)
	if err != nil {
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
	http.Error(writer, "Authentication required.", http.StatusUnauthorized)
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
					"operator": map[string]any{"type": "string", "enum": operators()},
					"keyword":  map[string]any{"type": "string"},
				},
				"required":             []string{"name", "keyword"},
				"additionalProperties": false,
			},
		},
	}, []string{"source_url", "outputs"})
}

func operators() []string {
	return []string{string(domain.FilterContains), string(domain.FilterStartsWith), string(domain.FilterEndsWith), string(domain.FilterEquals)}
}
