package source

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHTTPSourceRejectsLoopback(t *testing.T) {
	client := NewHTTPSource(&http.Client{})
	if _, _, err := client.Get(context.Background(), "http://127.0.0.1:1/feed.xml"); err == nil {
		t.Fatal("expected loopback destination to be rejected")
	}
}

func TestHTTPSourceRejectsUnsafeURLs(t *testing.T) {
	for _, sourceURL := range []string{
		"ftp://feed.example/feed.xml",
		"https://user:password@feed.example/feed.xml",
	} {
		_, _, err := NewHTTPSource(&http.Client{}).Get(context.Background(), sourceURL)
		if err == nil {
			t.Fatalf("expected unsafe URL to be rejected")
		}
		if strings.Contains(err.Error(), sourceURL) {
			t.Fatalf("error exposes source URL: %v", err)
		}
	}
}

func TestBlockedIP(t *testing.T) {
	tests := []struct {
		name string
		ip   netip.Addr
		want bool
	}{
		{name: "public IPv4", ip: netip.MustParseAddr("8.8.8.8"), want: false},
		{name: "loopback IPv4", ip: netip.MustParseAddr("127.0.0.1"), want: true},
		{name: "RFC1918 IPv4", ip: netip.MustParseAddr("192.168.1.1"), want: true},
		{name: "link-local IPv4", ip: netip.MustParseAddr("169.254.1.1"), want: true},
		{name: "metadata IPv4", ip: netip.MustParseAddr("169.254.169.254"), want: true},
		{name: "CGNAT IPv4", ip: netip.MustParseAddr("100.64.0.1"), want: true},
		{name: "unspecified IPv4", ip: netip.MustParseAddr("0.0.0.0"), want: true},
		{name: "multicast IPv4", ip: netip.MustParseAddr("224.0.0.1"), want: true},
		{name: "public IPv6", ip: netip.MustParseAddr("2001:4860:4860::8888"), want: false},
		{name: "loopback IPv6", ip: netip.MustParseAddr("::1"), want: true},
		{name: "link-local IPv6", ip: netip.MustParseAddr("fe80::1"), want: true},
		{name: "unspecified IPv6", ip: netip.MustParseAddr("::"), want: true},
		{name: "multicast IPv6", ip: netip.MustParseAddr("ff02::1"), want: true},
		{name: "mapped IPv6", ip: netip.MustParseAddr("::ffff:8.8.8.8"), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := blockedIP(test.ip); got != test.want {
				t.Fatalf("blockedIP(%v) = %t, want %t", test.ip, got, test.want)
			}
		})
	}
}

func TestSafeDialContextChecksMixedDNSResultsBeforeDialing(t *testing.T) {
	var dialed []string
	dial := safeDialContextWithLookup(
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("100.64.0.1"),
				netip.MustParseAddr("93.184.216.34"),
			}, nil
		},
		func(_ context.Context, _ string, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, nil
		},
	)

	if _, err := dial(context.Background(), "tcp", "feed.example:443"); err != nil {
		t.Fatalf("safe dial failed: %v", err)
	}
	if len(dialed) != 1 || dialed[0] != "93.184.216.34:443" {
		t.Fatalf("dialed addresses = %v, want only allowed address", dialed)
	}
}

func TestSafeDialContextHidesResolvedAddresses(t *testing.T) {
	dial := safeDialContextWithLookup(
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("blocked address was dialed")
			return nil, nil
		},
	)

	_, err := dial(context.Background(), "tcp", "metadata.example:80")
	if err == nil {
		t.Fatal("expected blocked destination error")
	}
	if strings.Contains(err.Error(), "169.254.169.254") || strings.Contains(err.Error(), "metadata.example") {
		t.Fatalf("error exposes destination: %v", err)
	}
}

func TestHTTPSourceRedirectValidation(t *testing.T) {
	source := NewHTTPSource(&http.Client{Timeout: time.Second})
	for _, test := range []struct {
		name string
		url  string
	}{
		{name: "non HTTP scheme", url: "ftp://feed.example/feed.xml"},
		{name: "userinfo", url: "https://user:password@feed.example/feed.xml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, test.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = source.client.CheckRedirect(request, nil)
			if err == nil {
				t.Fatal("expected redirect to be rejected")
			}
			if strings.Contains(err.Error(), test.url) {
				t.Fatalf("error exposes redirect URL: %v", err)
			}
		})
	}
}

func TestHTTPSourcePreservesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := NewHTTPSource(&http.Client{Timeout: time.Second}).Get(ctx, "http://example.com/feed.xml")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "example.com") {
		t.Fatalf("error exposes source URL: %v", err)
	}
}

func TestHTTPSourceTracesUpstreamStatusWithoutSourceURL(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := trace.NewTracerProvider(trace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})

	source := &HTTPSource{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader("private upstream response")),
		}, nil
	})}}
	_, _, err := source.Get(context.Background(), "https://private.example/rss")
	if err == nil {
		t.Fatal("expected upstream status error")
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected one source span, got %d", len(spans))
	}
	span := spans[0]
	if span.Name != "source.get" || len(span.Events) != 1 {
		t.Fatalf("unexpected source span: %#v", span)
	}
	var status int64
	for _, attribute := range span.Attributes {
		if attribute.Key == sourceStatusCodeAttr {
			status = attribute.Value.AsInt64()
		}
	}
	if status != http.StatusBadGateway {
		t.Fatalf("source status = %d, want %d", status, http.StatusBadGateway)
	}
	for _, attribute := range span.Events[0].Attributes {
		if attribute.Key == "exception.message" && strings.Contains(attribute.Value.AsString(), "private.example") {
			t.Fatalf("source span exposed source URL: %#v", span.Events)
		}
	}
}
