package domain

import (
	"strings"
	"time"
	"unicode"
)

type FilterOperator string

const (
	FilterContains   FilterOperator = "contains"
	FilterStartsWith FilterOperator = "starts_with"
	FilterEndsWith   FilterOperator = "ends_with"
	FilterEquals     FilterOperator = "equals"
)

func (operator FilterOperator) Valid() bool {
	switch operator {
	case FilterContains, FilterStartsWith, FilterEndsWith, FilterEquals:
		return true
	default:
		return false
	}
}

func (operator FilterOperator) Label() string {
	switch operator {
	case FilterStartsWith:
		return "Starts with"
	case FilterEndsWith:
		return "Ends with"
	case FilterEquals:
		return "Equals"
	case FilterContains:
		return "Contains"
	default:
		return string(operator)
	}
}

type Feed struct {
	ID        int64
	TenantID  int64
	Name      string
	Slug      string
	Path      string
	SourceURL string
	Operator  FilterOperator
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
