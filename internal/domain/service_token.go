package domain

import (
	"errors"
	"sort"
	"strings"
	"time"
)

var ErrInvalidServiceToken = errors.New("invalid service token")

const (
	ScopeFeedsRead    = "feeds:read"
	ScopeFeedsPreview = "feeds:preview"
	ScopeFeedsWrite   = "feeds:write"
	ScopeFeedsDelete  = "feeds:delete"
	ScopeTokensManage = "tokens:manage"
)

var serviceTokenScopes = []string{
	ScopeFeedsRead,
	ScopeFeedsPreview,
	ScopeFeedsWrite,
	ScopeFeedsDelete,
	ScopeTokensManage,
}

// AllServiceTokenScopes returns a fresh list suitable for issuing a fully
// privileged bootstrap token.
func AllServiceTokenScopes() []string {
	return append([]string(nil), serviceTokenScopes...)
}

type ServiceToken struct {
	ID        int64
	UserID    int64
	Username  string
	Name      string
	Scopes    []string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

func ParseServiceTokenScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, ErrInvalidServiceToken
	}
	allowed := make(map[string]int, len(serviceTokenScopes))
	for i, scope := range serviceTokenScopes {
		allowed[scope] = i
	}
	seen := make(map[string]struct{}, len(scopes))
	parsed := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if _, ok := allowed[scope]; !ok {
			return nil, ErrInvalidServiceToken
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		parsed = append(parsed, scope)
	}
	if len(parsed) == 0 {
		return nil, ErrInvalidServiceToken
	}
	sort.Slice(parsed, func(i, j int) bool { return allowed[parsed[i]] < allowed[parsed[j]] })
	return parsed, nil
}
