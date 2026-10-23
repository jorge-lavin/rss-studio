package source

import (
	"context"
	"net/http"
	"testing"
)

func TestHTTPSourceRejectsLoopback(t *testing.T) {
	client := NewHTTPSource(&http.Client{})
	if _, _, err := client.Get(context.Background(), "http://127.0.0.1:1/feed.xml"); err == nil {
		t.Fatal("expected loopback destination to be rejected")
	}
}
