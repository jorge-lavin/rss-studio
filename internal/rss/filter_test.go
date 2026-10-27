package rss

import (
	"errors"
	"reflect"
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

func TestFilterRejectsHostileXMLDepth(t *testing.T) {
	source := `<rss version="2.0"><channel><title>Source</title>` +
		strings.Repeat("<nested>", maxXMLDepth+1) +
		strings.Repeat("</nested>", maxXMLDepth+1) +
		`</channel></rss>`

	_, _, err := Filter([]byte(source), "foo", "Feed")
	if err == nil {
		t.Fatal("expected excessive XML depth to fail")
	}
	if !errors.Is(err, etree.ErrMaxDepth) {
		t.Fatalf("expected etree max-depth error, got %v", err)
	}
	if !strings.Contains(err.Error(), "XML depth exceeds limit of 256") {
		t.Fatalf("expected explicit depth limit error, got %v", err)
	}
}

func TestFilterMany(t *testing.T) {
	source := []byte(`<?xml version="1.0"?><rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel><title>Original</title><link>https://example.test/feed</link><item><title>AI weekly</title><guid>one</guid><enclosure url="https://media.test/one.mp3" type="audio/mpeg"/><itunes:duration>10</itunes:duration></item><item><title>AI news</title><guid>two</guid><enclosure url="https://media.test/two.mp3" type="audio/mpeg"/></item><item><title>Other show</title><guid>three</guid></item></channel></rss>`)

	results, err := FilterMany(source, []FilterRule{
		{Name: "AI", Keyword: "ai"},
		{Name: "News", Keyword: "news"},
		{Name: "Missing", Keyword: "missing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	want := []struct {
		name  string
		count int
		guids []string
	}{
		{name: "AI", count: 2, guids: []string{"one", "two"}},
		{name: "News", count: 1, guids: []string{"two"}},
		{name: "Missing", count: 0},
	}
	for i, test := range want {
		result := results[i]
		if result.Name != test.name || result.Count != test.count {
			t.Fatalf("result %d: got name %q and count %d", i, result.Name, result.Count)
		}

		document := etree.NewDocument()
		if err := document.ReadFromBytes(result.Feed); err != nil {
			t.Fatal(err)
		}
		channel := document.FindElement("/rss/channel")
		if got := channel.SelectElement("title").Text(); got != test.name {
			t.Fatalf("result %d: expected title %q, got %q", i, test.name, got)
		}
		if got := channel.SelectElement("link").Text(); got != "https://example.test/feed" {
			t.Fatalf("result %d: channel metadata was not preserved", i)
		}

		items := channel.SelectElements("item")
		if len(items) != len(test.guids) {
			t.Fatalf("result %d: expected %d items, got %d", i, len(test.guids), len(items))
		}
		for j, guid := range test.guids {
			if got := items[j].SelectElement("guid").Text(); got != guid {
				t.Fatalf("result %d item %d: expected guid %q, got %q", i, j, guid, got)
			}
		}
	}
	if !strings.Contains(string(results[0].Feed), `url="https://media.test/one.mp3"`) ||
		!strings.Contains(string(results[0].Feed), "itunes:duration") {
		t.Fatal("matching item data was not preserved")
	}
}

func TestFilterManyUsesEachRuleOperator(t *testing.T) {
	source := []byte(`<rss version="2.0"><channel><title>Source</title><item><title>AI weekly</title></item><item><title>Weekly AI</title></item><item><title>AI</title></item><item><title>Other show</title></item></channel></rss>`)
	results, err := FilterMany(source, []FilterRule{
		{Name: "Starts", Operator: domain.FilterStartsWith, Keyword: "ai"},
		{Name: "Ends", Operator: domain.FilterEndsWith, Keyword: "ai"},
		{Name: "Equals", Operator: domain.FilterEquals, Keyword: "ai"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := []int{results[0].Count, results[1].Count, results[2].Count}; !reflect.DeepEqual(got, []int{2, 2, 1}) {
		t.Fatalf("unexpected operator counts: %v", got)
	}
}

func TestPreviewManyReturnsMetadataWithoutFeeds(t *testing.T) {
	source := []byte(`<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel><title>Source</title><itunes:image href="https://images.test/show.jpg"/><item><title>AI one</title><itunes:image href="https://images.test/episode.jpg"/></item><item><title>Other</title></item></channel></rss>`)
	results, err := PreviewMany(source, []FilterRule{{Name: "AI", Keyword: "ai"}, {Name: "Other", Keyword: "other"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Name != "AI" || results[0].Count != 1 || len(results[0].Titles) != 1 || results[1].Count != 1 {
		t.Fatalf("unexpected preview results: %#v", results)
	}
	if results[0].FeedImageURL != "https://images.test/show.jpg" || len(results[0].Episodes) != 1 || results[0].Episodes[0].Title != "AI one" || results[0].Episodes[0].ImageURL != "https://images.test/episode.jpg" {
		t.Fatalf("preview image metadata was not extracted: %#v", results[0])
	}
}
