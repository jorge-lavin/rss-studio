package rss

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/beevik/etree"

	"rss-studio/internal/domain"
)

const (
	maxSourceFeedBytes = 25 << 20
	maxXMLDepth        = 256
)

type diagnosticError struct {
	class   string
	message string
	cause   error
	reason  string
	line    int
}

func (e *diagnosticError) Error() string { return e.message }

func (e *diagnosticError) Unwrap() error { return e.cause }

func (e *diagnosticError) FailureClass() string { return e.class }

func (e *diagnosticError) FailureReason() string { return e.reason }

func (e *diagnosticError) FailureLine() int { return e.line }

func newDiagnosticError(class, message string, cause error) error {
	reason, line := failureDetails(cause)
	return &diagnosticError{class: class, message: message, cause: cause, reason: reason, line: line}
}

func failureDetails(err error) (reason string, line int) {
	if err == nil {
		return "", 0
	}
	var syntaxErr *xml.SyntaxError
	if errors.As(err, &syntaxErr) {
		line = syntaxErr.Line
		switch {
		case strings.Contains(syntaxErr.Msg, "unexpected EOF"):
			return "unexpected_eof", line
		case strings.Contains(syntaxErr.Msg, "unexpected end element"):
			return "unexpected_end_element", line
		case strings.Contains(syntaxErr.Msg, "expected element name"):
			return "expected_element_name", line
		case strings.Contains(syntaxErr.Msg, "closed by"):
			return "mismatched_end_element", line
		default:
			return "syntax_error", line
		}
	}
	if err.Error() == "etree: invalid XML format" {
		return "invalid_format", 0
	}
	return "parse_error", 0
}

// FilterRule names one filtered output and its title conditions.
type FilterRule struct {
	Name     string
	Operator domain.FilterOperator
	Keyword  string
	Match    domain.FilterMatch
	Filters  []domain.FilterCondition
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
	Name         string
	Count        int
	Titles       []string
	FeedImageURL string
	Episodes     []EpisodePreview
}

// EpisodePreview contains the small amount of episode metadata needed by the
// preview UI without returning or generating the full feed.
type EpisodePreview struct {
	Title    string
	ImageURL string
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
		match, filters := filterConditions(rule)
		result := PreviewResult{
			Name:         rule.Name,
			Titles:       make([]string, 0, 5),
			FeedImageURL: imageURL(channel),
			Episodes:     make([]EpisodePreview, 0, 5),
		}
		for _, item := range channel.SelectElements("item") {
			title := item.SelectElement("title")
			if title == nil || !matchesFilters(strings.ToLower(strings.TrimSpace(title.Text())), match, filters) {
				continue
			}
			result.Count++
			if len(result.Titles) < 5 {
				result.Titles = append(result.Titles, title.Text())
				result.Episodes = append(result.Episodes, EpisodePreview{Title: title.Text(), ImageURL: imageURL(item)})
			}
		}
		results = append(results, result)
	}
	return results, nil
}

func imageURL(element *etree.Element) string {
	for _, child := range element.ChildElements() {
		tag := child.Tag
		if child.Space != "" {
			tag = child.Space + ":" + tag
		}
		switch tag {
		case "itunes:image":
			if value := child.SelectAttrValue("href", ""); value != "" {
				return strings.TrimSpace(value)
			}
		case "image":
			if nested := child.SelectElement("url"); nested != nil {
				return strings.TrimSpace(nested.Text())
			}
		case "media:thumbnail":
			if value := child.SelectAttrValue("url", ""); value != "" {
				return strings.TrimSpace(value)
			}
		case "media:content":
			contentType := strings.ToLower(strings.TrimSpace(child.SelectAttrValue("type", "")))
			if strings.EqualFold(child.SelectAttrValue("medium", ""), "image") || strings.HasPrefix(contentType, "image/") {
				return strings.TrimSpace(child.SelectAttrValue("url", ""))
			}
		}
	}
	return ""
}

// FilterMany generates one filtered RSS feed for each rule, in rule order.
func FilterMany(source []byte, rules []FilterRule) ([]FilterResult, error) {
	document, err := parseDocument(source)
	if err != nil {
		return nil, err
	}

	results := make([]FilterResult, 0, len(rules))
	for _, rule := range rules {
		match, filters := filterConditions(rule)
		feed, count, titles, err := filterDocument(document.Copy(), match, filters, rule.Name)
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
	return FilterWithConditions(source, domain.FilterAny, []domain.FilterCondition{{Operator: operator, Keyword: keyword}}, outputTitle)
}

func FilterWithConditions(source []byte, match domain.FilterMatch, filters []domain.FilterCondition, outputTitle string) ([]byte, int, error) {
	document, err := parseDocument(source)
	if err != nil {
		return nil, 0, err
	}

	feed, count, _, err := filterDocument(document, match, filters, outputTitle)
	return feed, count, err
}

func parseDocument(source []byte) (*etree.Document, error) {
	if len(source) > maxSourceFeedBytes {
		return nil, newDiagnosticError("input_too_large", fmt.Sprintf("parse source feed: input exceeds %d-byte limit", maxSourceFeedBytes), nil)
	}
	document := etree.NewDocument()
	document.ReadSettings = etree.ReadSettings{
		MaxDepth: maxXMLDepth,
	}
	if err := validateXML(source); err != nil {
		if errors.Is(err, etree.ErrMaxDepth) {
			return nil, newDiagnosticError("xml_depth_limit", fmt.Sprintf("parse source feed: XML depth exceeds limit of %d: %v", maxXMLDepth, err), err)
		}
		return nil, newDiagnosticError("invalid_xml", fmt.Sprintf("parse source feed: %v", err), err)
	}
	if err := document.ReadFromBytes(source); err != nil {
		if errors.Is(err, etree.ErrMaxDepth) {
			return nil, newDiagnosticError("xml_depth_limit", fmt.Sprintf("parse source feed: XML depth exceeds limit of %d: %v", maxXMLDepth, err), err)
		}
		return nil, newDiagnosticError("invalid_xml", fmt.Sprintf("parse source feed: %v", err), err)
	}
	if document.FindElement("/rss/channel") == nil {
		return nil, newDiagnosticError("invalid_rss", "source is not an RSS 2.0 feed", nil)
	}
	return document, nil
}

func validateXML(source []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(source))
	if err := decoder.Decode(new(interface{})); err != nil {
		return err
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return etree.ErrXML
			}
		case xml.StartElement, xml.EndElement:
			return etree.ErrXML
		}
	}
}

func filterDocument(document *etree.Document, match domain.FilterMatch, filters []domain.FilterCondition, outputTitle string) ([]byte, int, []string, error) {
	channel := document.FindElement("/rss/channel")
	if channel == nil {
		return nil, 0, nil, newDiagnosticError("invalid_rss", "source is not an RSS 2.0 feed", nil)
	}

	matched := 0
	titles := make([]string, 0, 5)
	for _, item := range channel.SelectElements("item") {
		title := item.SelectElement("title")
		if title != nil && matchesFilters(strings.ToLower(strings.TrimSpace(title.Text())), match, filters) {
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
		return nil, 0, nil, newDiagnosticError("output_write", fmt.Sprintf("write filtered feed: %v", err), err)
	}
	return result, matched, titles, nil
}

func filterConditions(rule FilterRule) (domain.FilterMatch, []domain.FilterCondition) {
	if len(rule.Filters) > 0 {
		match := rule.Match
		if match == "" {
			match = domain.FilterAny
		}
		return match, rule.Filters
	}
	operator := rule.Operator
	if operator == "" {
		operator = domain.FilterContains
	}
	return domain.FilterAny, []domain.FilterCondition{{Operator: operator, Keyword: rule.Keyword}}
}

func matchesFilters(title string, match domain.FilterMatch, filters []domain.FilterCondition) bool {
	if len(filters) == 0 {
		return false
	}
	if match == domain.FilterAll {
		for _, filter := range filters {
			if !matches(title, filter.Operator, strings.ToLower(strings.TrimSpace(filter.Keyword))) {
				return false
			}
		}
		return true
	}
	for _, filter := range filters {
		if matches(title, filter.Operator, strings.ToLower(strings.TrimSpace(filter.Keyword))) {
			return true
		}
	}
	return false
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
