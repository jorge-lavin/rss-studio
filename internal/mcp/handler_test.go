package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/cache"
	"rss-studio/internal/domain"
	"rss-studio/internal/httpapi"
	"rss-studio/internal/policy"
	"rss-studio/internal/rss"
	"rss-studio/internal/storage"
)

type sourceStub struct{}

func (sourceStub) Head(context.Context, string) (time.Time, error) { return time.Time{}, nil }

func (sourceStub) Get(context.Context, string) ([]byte, time.Time, error) {
	return []byte(`<rss version="2.0"><channel><title>Source</title><item><title>Keep foo</title><guid>1</guid></item><item><title>Drop</title><guid>2</guid></item></channel></rss>`), time.Time{}, nil
}

type authenticatorStub struct {
	user domain.User
	err  error
}

func (a authenticatorStub) UserFromRequest(*http.Request) (domain.User, error) {
	return a.user, a.err
}

func (a authenticatorStub) IdentityFromRequest(*http.Request) (policy.Identity, error) {
	return policy.Identity{
		User:           a.user,
		CredentialHash: "test-credential",
		Scopes:         []string{domain.ScopeFeedsRead, domain.ScopeFeedsPreview, domain.ScopeFeedsWrite, domain.ScopeFeedsDelete},
		ServiceToken:   true,
	}, a.err
}

type rpcResponseForTest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func newTestHandler(t *testing.T) (http.Handler, domain.User) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	owner, err := store.CreateUser("admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewServiceWithOperator(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.FilterWithOperator)
	return NewHandler(service, authenticatorStub{user: owner}), owner
}

func newAuthenticatedMCPHandler(t *testing.T, quotas application.QuotaConfig, scopes []string, now func() time.Time) (http.Handler, *storage.SQLiteStore, domain.User, string, *httpapi.Authenticator) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	owner, err := store.CreateUser("admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{JWTSecret: strings.Repeat("x", 32), UserStore: store})
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := store.CreateServiceToken(owner, "mcp-agent", scopes, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewServiceWithOperatorAndQuotas(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.FilterWithOperator, quotas)
	return NewHandlerWithClock(service, auth, now), store, owner, raw, auth
}

func callRPC(t *testing.T, handler http.Handler, id int, method string, params any) rpcResponseForTest {
	return callRPCWithToken(t, handler, "test-token", id, method, params)
}

func callRPCWithToken(t *testing.T, handler http.Handler, token string, id int, method string, params any) rpcResponseForTest {
	t.Helper()
	response := callRPCWithTokenAllowError(t, handler, token, id, method, params)
	if response.Error != nil {
		t.Fatalf("MCP %s failed: error code %d", method, response.Error.Code)
	}
	return response
}

func callRPCWithTokenAllowError(t *testing.T, handler http.Handler, token string, id int, method string, params any) rpcResponseForTest {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://rss.test/mcp", strings.NewReader(string(body)))
	request.Header.Set("MCP-Protocol-Version", protocolVersion)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("MCP %s returned %d: %s", method, recorder.Code, recorder.Body.String())
	}
	var response rpcResponseForTest
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.JSONRPC != "2.0" {
		t.Fatalf("MCP %s failed: %s", method, recorder.Body.String())
	}
	return response
}

func toolCall(t *testing.T, handler http.Handler, id int, name string, arguments any) json.RawMessage {
	t.Helper()
	response := callRPC(t, handler, id, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	return response.Result
}

func toolText(t *testing.T, result json.RawMessage) string {
	t.Helper()
	value := toolResultFor(t, result)
	if value.IsError || len(value.Content) != 1 {
		t.Fatalf("unexpected tool result: %s", result)
	}
	return value.Content[0].Text
}

func toolResultFor(t *testing.T, result json.RawMessage) toolResult {
	t.Helper()
	var value toolResult
	if err := json.Unmarshal(result, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestHandlerRequiresAuthentication(t *testing.T) {
	handler := NewHandler(nil, authenticatorStub{err: errors.New("invalid token")})
	request := httptest.NewRequest(http.MethodPost, "http://rss.test/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("expected authenticated MCP endpoint, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerInitializesAndListsTools(t *testing.T) {
	handler, _ := newTestHandler(t)
	response := callRPC(t, handler, 1, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	var initialized initializeResult
	if err := json.Unmarshal(response.Result, &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.ProtocolVersion != protocolVersion || initialized.ServerInfo.Name != "rss-studio" {
		t.Fatalf("unexpected initialize result: %#v", initialized)
	}

	response = callRPC(t, handler, 2, "tools/list", nil)
	var listed toolsListResult
	if err := json.Unmarshal(response.Result, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 5 || listed.Tools[0].Name != "list_feeds" {
		t.Fatalf("unexpected tools: %#v", listed.Tools)
	}
}

func TestHandlerManagesFeedsThroughTools(t *testing.T) {
	handler, owner := newTestHandler(t)
	result := toolCall(t, handler, 1, "create_feed", map[string]any{
		"name":       "Keep feed",
		"source_url": "https://example.test/rss",
		"operator":   "starts_with",
		"keyword":    "Keep",
	})
	var created feedOutput
	if err := json.Unmarshal([]byte(toolText(t, result)), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "Keep feed" || created.Operator != "starts_with" || created.URL != "http://rss.test"+created.Path {
		t.Fatalf("unexpected created feed: %#v", created)
	}

	result = toolCall(t, handler, 2, "list_feeds", map[string]any{})
	var listed []feedOutput
	if err := json.Unmarshal([]byte(toolText(t, result)), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != created.Name || listed[0].SourceURL != created.SourceURL {
		t.Fatalf("unexpected listed feeds: %#v", listed)
	}

	toolCall(t, handler, 3, "delete_feed", map[string]any{"path": created.Path})
	result = toolCall(t, handler, 4, "list_feeds", map[string]any{})
	var afterDelete []feedOutput
	if err := json.Unmarshal([]byte(toolText(t, result)), &afterDelete); err != nil {
		t.Fatal(err)
	}
	if len(afterDelete) != 0 {
		t.Fatalf("feed was not deleted: %#v", afterDelete)
	}
	if owner.ID == 0 {
		t.Fatal("test owner was not persisted")
	}
}

func TestHandlerPreviewsWithoutPersisting(t *testing.T) {
	handler, _ := newTestHandler(t)
	result := toolCall(t, handler, 1, "preview_feed", map[string]any{
		"source_url": "https://example.test/rss",
		"outputs": []map[string]any{{
			"name": "Keep", "operator": "starts_with", "keyword": "keep",
		}},
	})
	var previews []previewOutput
	if err := json.Unmarshal([]byte(toolText(t, result)), &previews); err != nil {
		t.Fatal(err)
	}
	if len(previews) != 1 || previews[0].Count != 1 || len(previews[0].SampleTitles) != 1 || previews[0].SampleTitles[0] != "Keep foo" {
		t.Fatalf("unexpected preview: %#v", previews)
	}
}

func TestHandlerRejectsUnsupportedProtocolVersion(t *testing.T) {
	handler, _ := newTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "http://rss.test/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("MCP-Protocol-Version", "2099-01-01")
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected unsupported protocol version to fail, got %d", recorder.Code)
	}
}

func TestHandlerReturnsProtocolErrorsForMalformedRequests(t *testing.T) {
	handler, _, _, token, _ := newAuthenticatedMCPHandler(t, application.DefaultQuotaConfig(), []string{domain.ScopeFeedsRead}, time.Now)
	request := httptest.NewRequest(http.MethodPost, "http://rss.test/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1`))
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":-32700`) {
		t.Fatalf("malformed JSON did not return parse error: %d %s", recorder.Code, recorder.Body.String())
	}

	response := callRPCWithTokenAllowError(t, handler, token, 2, "unknown/method", nil)
	if response.Error == nil || response.Error.Code != -32601 {
		t.Fatalf("unknown method returned %#v", response)
	}
	response = callRPCWithTokenAllowError(t, handler, token, 3, "tools/call", map[string]any{"extra": true})
	if response.Error == nil || response.Error.Code != -32602 {
		t.Fatalf("malformed tool params returned %#v", response)
	}
}

func TestHandlerRejectsJWTAndEnforcesToolScopes(t *testing.T) {
	handler, _, _, serviceToken, auth := newAuthenticatedMCPHandler(t, application.DefaultQuotaConfig(), []string{domain.ScopeFeedsRead}, time.Now)
	jwt, _, err := auth.IssueToken("admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://rss.test/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer "+jwt)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || bytes.Contains(recorder.Body.Bytes(), []byte(jwt)) {
		t.Fatalf("JWT was accepted or leaked: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	response := callRPCWithTokenAllowError(t, handler, serviceToken, 2, "tools/call", map[string]any{
		"name": "create_feed", "arguments": map[string]any{
			"name": "No write", "source_url": "https://example.test/rss", "keyword": "foo",
		},
	})
	if response.Error == nil || response.Error.Code != -32003 {
		t.Fatalf("missing write scope returned %#v", response)
	}
	if bytes.Contains(response.Result, []byte(serviceToken)) {
		t.Fatal("scope error leaked service token")
	}
}

func TestHandlerRejectsUnknownAndMalformedToolArguments(t *testing.T) {
	handler, _, _, token, _ := newAuthenticatedMCPHandler(t, application.DefaultQuotaConfig(), []string{domain.ScopeFeedsRead}, time.Now)
	response := callRPCWithToken(t, handler, token, 1, "tools/call", map[string]any{
		"name": "list_feeds", "arguments": map[string]any{"extra": true},
	})
	result := toolResultFor(t, response.Result)
	if !result.IsError || !strings.Contains(result.Content[0].Text, "Invalid tool arguments") {
		t.Fatalf("unknown argument was accepted: %#v", result)
	}
}

func TestHandlerMutationIdempotencyReplaysAndConflicts(t *testing.T) {
	handler, store, owner, token, _ := newAuthenticatedMCPHandler(t, application.DefaultQuotaConfig(), []string{domain.ScopeFeedsWrite}, time.Now)
	arguments := map[string]any{
		"name": "Idempotent MCP feed", "source_url": "https://example.test/rss", "keyword": "foo", "idempotency_key": "mcp-create-key",
	}
	first := toolCallWithToken(t, handler, token, 1, "create_feed", arguments)
	retry := toolCallWithToken(t, handler, token, 2, "create_feed", arguments)
	if !bytes.Equal(first, retry) {
		t.Fatalf("idempotent retry changed result: first=%s retry=%s", first, retry)
	}
	changed := toolCallWithToken(t, handler, token, 3, "create_feed", map[string]any{
		"name": "Changed MCP feed", "source_url": "https://example.test/rss", "keyword": "foo", "idempotency_key": "mcp-create-key",
	})
	result := toolResultFor(t, changed)
	if !result.IsError || !strings.Contains(result.Content[0].Text, "Idempotency key") || strings.Contains(result.Content[0].Text, "mcp-create-key") {
		t.Fatalf("changed idempotent request was unsafe: %#v", result)
	}
	feeds, err := store.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 {
		t.Fatalf("idempotent MCP create created %d feeds", len(feeds))
	}
}

func TestHandlerMCPQuotaFailureIsSafeToolError(t *testing.T) {
	limits := application.DefaultQuotaConfig()
	limits.PreviewsPerDay = 1
	handler, _, _, token, _ := newAuthenticatedMCPHandler(t, limits, []string{domain.ScopeFeedsPreview}, time.Now)
	arguments := map[string]any{
		"source_url": "https://example.test/private-source", "outputs": []map[string]any{{"name": "Foo", "keyword": "foo"}},
	}
	toolCallWithToken(t, handler, token, 1, "preview_feed", arguments)
	result := toolResultFor(t, toolCallWithToken(t, handler, token, 2, "preview_feed", arguments))
	if !result.IsError || result.Content[0].Text != "Quota exceeded." || strings.Contains(result.Content[0].Text, "private-source") {
		t.Fatalf("unsafe quota error: %#v", result)
	}
}

func TestHandlerMCPRateLimitsAggregateAccountAcrossServiceTokens(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	handler, store, owner, firstToken, _ := newAuthenticatedMCPHandler(t, application.DefaultQuotaConfig(), []string{domain.ScopeFeedsPreview}, func() time.Time { return now })
	_, secondToken, err := store.CreateServiceToken(owner, "second-mcp-agent", []string{domain.ScopeFeedsPreview}, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	arguments := map[string]any{
		"source_url": "https://example.test/rss", "outputs": []map[string]any{{"name": "Foo", "keyword": "foo"}},
	}
	for i := 0; i < policy.PreviewRateLimit; i++ {
		token := firstToken
		if i%2 == 1 {
			token = secondToken
		}
		result := toolResultFor(t, toolCallWithToken(t, handler, token, i+1, "preview_feed", arguments))
		if result.IsError {
			t.Fatalf("preview %d was limited unexpectedly: %#v", i, result)
		}
	}
	result := toolResultFor(t, toolCallWithToken(t, handler, secondToken, 99, "preview_feed", arguments))
	if !result.IsError || result.Content[0].Text != "Rate limit exceeded. Try again later." {
		t.Fatalf("unexpected safe rate-limit error: %#v", result)
	}
}

func toolCallWithToken(t *testing.T, handler http.Handler, token string, id int, name string, arguments any) json.RawMessage {
	t.Helper()
	response := callRPCWithToken(t, handler, token, id, "tools/call", map[string]any{
		"name": name, "arguments": arguments,
	})
	return response.Result
}
