package httpapi

import (
	"strings"
	"unicode"

	"rss-studio/internal/domain"
)

const maxFilterSummaryLength = 80

func filterSummary(match domain.FilterMatch, filters []domain.FilterCondition) string {
	if len(filters) == 0 {
		return match.Label() + " filters"
	}

	separator := " or "
	if match == domain.FilterAll {
		separator = " and "
	}
	parts := make([]string, len(filters))
	for i, filter := range filters {
		parts[i] = filter.Operator.Label() + " “" + filter.Keyword + "”"
	}
	return truncateFilterSummary(strings.Join(parts, separator))
}

func truncateFilterSummary(value string) string {
	runes := []rune(value)
	if len(runes) <= maxFilterSummaryLength {
		return value
	}
	return strings.TrimRightFunc(string(runes[:maxFilterSummaryLength-3]), unicode.IsSpace) + "..."
}
