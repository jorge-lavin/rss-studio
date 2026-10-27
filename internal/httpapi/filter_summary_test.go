package httpapi

import (
	"strings"
	"testing"

	"rss-studio/internal/domain"
)

func TestFilterSummaryDescribesCombinedConditions(t *testing.T) {
	filters := []domain.FilterCondition{
		{Operator: domain.FilterStartsWith, Keyword: "Keep"},
		{Operator: domain.FilterEquals, Keyword: "Drop"},
	}

	if got := filterSummary(domain.FilterAny, filters); got != "Starts with “Keep” or Equals “Drop”" {
		t.Fatalf("unexpected any-filter summary: %q", got)
	}
	if got := filterSummary(domain.FilterAll, filters); got != "Starts with “Keep” and Equals “Drop”" {
		t.Fatalf("unexpected all-filter summary: %q", got)
	}
}

func TestFilterSummaryTruncatesUIText(t *testing.T) {
	got := filterSummary(domain.FilterAny, []domain.FilterCondition{{
		Operator: domain.FilterContains,
		Keyword:  strings.Repeat("long keyword ", 10),
	}, {
		Operator: domain.FilterEquals,
		Keyword:  "second",
	}})

	if len([]rune(got)) > maxFilterSummaryLength || !strings.HasSuffix(got, "...") {
		t.Fatalf("summary was not truncated to UI limit: %q", got)
	}
}
