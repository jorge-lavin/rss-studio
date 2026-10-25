package domain

import (
	"strings"
	"time"
	"unicode"
)

type Feed struct {
	ID        int64
	TenantID  int64
	Name      string
	Slug      string
	Path      string
	SourceURL string
	Keyword   string
	CreatedAt time.Time
}

func Slugify(value string) string {
	var result strings.Builder
	previousDash := false
	for _, char := range strings.ToLower(value) {
		switch {
		case unicode.IsLetter(char) || unicode.IsDigit(char):
			result.WriteRune(char)
			previousDash = false
		case result.Len() > 0 && !previousDash:
			result.WriteByte('-')
			previousDash = true
		}
	}
	return strings.Trim(result.String(), "-")
}
