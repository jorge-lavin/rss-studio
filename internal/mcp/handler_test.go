package mcp

import (
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

func callRPC(t *testing.T, handler http.Handler, id int, method string, params any) rpcResponseForTest {
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
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("MCP %s returned %d: %s", method, recorder.Code, recorder.Body.String())
	}
	var response rpcResponseForTest
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.JSONRPC != "2.0" || response.Error != nil {
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
	var value toolResult
	if err := json.Unmarshal(result, &value); err != nil {
		t.Fatal(err)
	}
	if value.IsError || len(value.Content) != 1 {
		t.Fatalf("unexpected tool result: %s", result)
	}
	return value.Content[0].Text
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
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected unsupported protocol version to fail, got %d", recorder.Code)
	}
}
