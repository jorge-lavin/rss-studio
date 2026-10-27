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

type FilterMatch string

const (
	FilterAny FilterMatch = "any"
	FilterAll FilterMatch = "all"
)

type FilterCondition struct {
	Operator FilterOperator `json:"operator"`
	Keyword  string         `json:"keyword"`
}

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

func (match FilterMatch) Valid() bool {
	return match == FilterAny || match == FilterAll
}

func (match FilterMatch) Label() string {
	if match == FilterAll {
		return "All"
	}
	return "Any"
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
	Match     FilterMatch
	Filters   []FilterCondition
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
