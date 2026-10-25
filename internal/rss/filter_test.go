package rss

import (
	"strings"
	"testing"

	"github.com/beevik/etree"

	"rss-studio/internal/domain"
)

func TestFilterKeepsMatchingItems(t *testing.T) {
	source := []byte(`<?xml version="1.0"?><rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel><title>Original</title><item><title>Foo weekly</title><guid>one</guid><enclosure url="https://media.test/one.mp3" type="audio/mpeg"/><itunes:duration>10</itunes:duration></item><item><title>Other show</title><guid>two</guid></item></channel></rss>`)

	result, count, err := Filter(source, "fOO", "Filtered Feed")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 match, got %d", count)
	}

	document := etree.NewDocument()
	if err := document.ReadFromBytes(result); err != nil {
		t.Fatal(err)
	}
	channel := document.FindElement("/rss/channel")
	if got := channel.SelectElement("title").Text(); got != "Filtered Feed" {
		t.Fatalf("expected output title, got %q", got)
	}
	items := channel.SelectElements("item")
	if len(items) != 1 || items[0].SelectElement("guid").Text() != "one" {
		t.Fatalf("unexpected filtered items: %s", result)
	}
	if !strings.Contains(string(result), "itunes:duration") {
		t.Fatal("podcast extension was not preserved")
	}
}

func TestFilterRejectsAtom(t *testing.T) {
	_, _, err := Filter([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"></feed>`), "foo", "Feed")
	if err == nil {
		t.Fatal("expected non-RSS feed to fail")
	}
}

func TestFilterOperators(t *testing.T) {
	source := []byte(`<rss version="2.0"><channel><title>Source</title><item><title>AI weekly</title></item><item><title>Weekly AI</title></item><item><title>AI</title></item><item><title>Other show</title></item></channel></rss>`)
	tests := []struct {
		operator domain.FilterOperator
		want     int
	}{
		{domain.FilterContains, 3},
		{domain.FilterStartsWith, 2},
		{domain.FilterEndsWith, 2},
		{domain.FilterEquals, 1},
	}
	for _, test := range tests {
		t.Run(test.operator.Label(), func(t *testing.T) {
			result, count, err := FilterWithOperator(source, test.operator, "ai", "Filtered")
			if err != nil {
				t.Fatal(err)
			}
			if count != test.want {
				t.Fatalf("expected %d matches, got %d: %s", test.want, count, result)
			}
		})
	}
}
