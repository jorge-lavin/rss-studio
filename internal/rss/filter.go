package rss

import (
	"fmt"
	"strings"

	"github.com/beevik/etree"

	"rss-studio/internal/domain"
)

func Filter(source []byte, keyword, outputTitle string) ([]byte, int, error) {
	return FilterWithOperator(source, domain.FilterContains, keyword, outputTitle)
}

func FilterWithOperator(source []byte, operator domain.FilterOperator, keyword, outputTitle string) ([]byte, int, error) {
	document := etree.NewDocument()
	if err := document.ReadFromBytes(source); err != nil {
		return nil, 0, fmt.Errorf("parse source feed: %w", err)
	}

	channel := document.FindElement("/rss/channel")
	if channel == nil {
		return nil, 0, fmt.Errorf("source is not an RSS 2.0 feed")
	}

	needle := strings.ToLower(strings.TrimSpace(keyword))
	matched := 0
	for _, item := range channel.SelectElements("item") {
		title := item.SelectElement("title")
		if title != nil && matches(strings.ToLower(strings.TrimSpace(title.Text())), operator, needle) {
			matched++
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
		return nil, 0, fmt.Errorf("write filtered feed: %w", err)
	}
	return result, matched, nil
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
