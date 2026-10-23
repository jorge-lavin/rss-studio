package rss

import (
	"strings"
	"testing"

	"github.com/beevik/etree"
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
