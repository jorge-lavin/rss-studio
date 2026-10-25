package domain

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseServiceTokenScopesCanonicalizesAndDeduplicates(t *testing.T) {
	scopes, err := ParseServiceTokenScopes([]string{" feeds:delete ", ScopeFeedsRead, ScopeFeedsRead})
	if err != nil {
		t.Fatal(err)
	}
	if expected := []string{ScopeFeedsRead, ScopeFeedsDelete}; !reflect.DeepEqual(scopes, expected) {
		t.Fatalf("unexpected scopes: %#v", scopes)
	}
}

func TestParseServiceTokenScopesRejectsUnknownAndEmptyScopes(t *testing.T) {
	for _, scopes := range [][]string{nil, {}, {"feeds:admin"}, {""}} {
		if _, err := ParseServiceTokenScopes(scopes); !errors.Is(err, ErrInvalidServiceToken) {
			t.Fatalf("expected invalid scopes for %#v, got %v", scopes, err)
		}
	}
}
