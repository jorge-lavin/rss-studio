package rss

import (
	"errors"
	"fmt"
	"strings"

	"github.com/beevik/etree"

	"rss-studio/internal/domain"
)

const (
	maxSourceFeedBytes = 25 << 20
	maxXMLDepth        = 256
)

// FilterRule names one filtered output and its title keyword.
type FilterRule struct {
	Name     string
	Operator domain.FilterOperator
	Keyword  string
}

// FilterResult contains one filtered feed and its number of matching items.
type FilterResult struct {
	Name   string
	Feed   []byte
	Count  int
	Titles []string
}

// PreviewResult contains matching episode metadata without a generated feed.
type PreviewResult struct {
	Name   string
	Count  int
	Titles []string
}

// PreviewMany parses one source feed and returns matching metadata for each rule.
func PreviewMany(source []byte, rules []FilterRule) ([]PreviewResult, error) {
	document, err := parseDocument(source)
	if err != nil {
		return nil, err
	}
	channel := document.FindElement("/rss/channel")
	results := make([]PreviewResult, 0, len(rules))
	for _, rule := range rules {
		needle := strings.ToLower(strings.TrimSpace(rule.Keyword))
		operator := rule.Operator
		if operator == "" {
			operator = domain.FilterContains
		}
		result := PreviewResult{Name: rule.Name, Titles: make([]string, 0, 5)}
		for _, item := range channel.SelectElements("item") {
			title := item.SelectElement("title")
			if title == nil || !matches(strings.ToLower(strings.TrimSpace(title.Text())), operator, needle) {
				continue
			}
			result.Count++
			if len(result.Titles) < 5 {
				result.Titles = append(result.Titles, title.Text())
			}
		}
		results = append(results, result)
	}
	return results, nil
}

// FilterMany generates one filtered RSS feed for each rule, in rule order.
func FilterMany(source []byte, rules []FilterRule) ([]FilterResult, error) {
	document, err := parseDocument(source)
	if err != nil {
		return nil, err
	}

	results := make([]FilterResult, 0, len(rules))
	for _, rule := range rules {
		operator := rule.Operator
		if operator == "" {
			operator = domain.FilterContains
		}
		feed, count, titles, err := filterDocument(document.Copy(), operator, rule.Keyword, rule.Name)
		if err != nil {
			return nil, err
		}
		results = append(results, FilterResult{
			Name:   rule.Name,
			Feed:   feed,
			Count:  count,
			Titles: titles,
		})
	}
	return results, nil
}

func Filter(source []byte, keyword, outputTitle string) ([]byte, int, error) {
	return FilterWithOperator(source, domain.FilterContains, keyword, outputTitle)
}

func FilterWithOperator(source []byte, operator domain.FilterOperator, keyword, outputTitle string) ([]byte, int, error) {
	document, err := parseDocument(source)
	if err != nil {
		return nil, 0, err
	}

	feed, count, _, err := filterDocument(document, operator, keyword, outputTitle)
	return feed, count, err
}

func parseDocument(source []byte) (*etree.Document, error) {
	if len(source) > maxSourceFeedBytes {
		return nil, fmt.Errorf("parse source feed: input exceeds %d-byte limit", maxSourceFeedBytes)
	}
	document := etree.NewDocument()
	document.ReadSettings = etree.ReadSettings{
		MaxDepth:      maxXMLDepth,
		ValidateInput: true,
	}
	if err := document.ReadFromBytes(source); err != nil {
		if errors.Is(err, etree.ErrMaxDepth) {
			return nil, fmt.Errorf("parse source feed: XML depth exceeds limit of %d: %w", maxXMLDepth, err)
		}
		return nil, fmt.Errorf("parse source feed: %w", err)
	}
	if document.FindElement("/rss/channel") == nil {
		return nil, fmt.Errorf("source is not an RSS 2.0 feed")
	}
	return document, nil
}

func filterDocument(document *etree.Document, operator domain.FilterOperator, keyword, outputTitle string) ([]byte, int, []string, error) {
	channel := document.FindElement("/rss/channel")
	if channel == nil {
		return nil, 0, nil, fmt.Errorf("source is not an RSS 2.0 feed")
	}

	needle := strings.ToLower(strings.TrimSpace(keyword))
	matched := 0
	titles := make([]string, 0, 5)
	for _, item := range channel.SelectElements("item") {
		title := item.SelectElement("title")
		if title != nil && matches(strings.ToLower(strings.TrimSpace(title.Text())), operator, needle) {
			matched++
			if len(titles) < 5 {
				titles = append(titles, title.Text())
			}
			continue
		}
		channel.RemoveChild(item)
	}

	if title := channel.SelectElement("title"); title != nil {
		title.SetText(outputTitle)
	}

	document.Indent(2)
	result, err := document.WriteToBytes()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("write filtered feed: %w", err)
	}
	return result, matched, titles, nil
}

func matches(title string, operator domain.FilterOperator, needle string) bool {
	switch operator {
	case domain.FilterStartsWith:
		return strings.HasPrefix(title, needle)
	case domain.FilterEndsWith:
		return strings.HasSuffix(title, needle)
	case domain.FilterEquals:
		return title == needle
	case domain.FilterContains:
		return strings.Contains(title, needle)
	default:
		return false
	}
}
