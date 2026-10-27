package source

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	maxFeedSize          = 25 << 20
	sourceTracerName     = "rss-studio/source"
	sourceMethodAttr     = "rss_studio.source.method"
	sourceStatusCodeAttr = "rss_studio.source.status_code"
	sourceBytesAttr      = "rss_studio.source.bytes"
	sourceErrorTypeAttr  = "rss_studio.source.error_type"
)

type HTTPSource struct {
	client *http.Client
}

func NewHTTPSource(client *http.Client) *HTTPSource {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if configured, ok := client.Transport.(*http.Transport); ok {
		transport = configured.Clone()
	}
	transport.Proxy = nil
	baseDial := transport.DialContext
	if baseDial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second}
		baseDial = dialer.DialContext
	}
	transport.DialContext = safeDialContext(baseDial)

	configured := *client
	configured.Transport = transport
	configured.CheckRedirect = func(request *http.Request, _ []*http.Request) error {
		if _, err := validateURL(request.URL.String()); err != nil {
			return fmt.Errorf("unsafe redirect: %w", err)
		}
		return nil
	}
	return &HTTPSource{client: &configured}
}

func (s *HTTPSource) Head(ctx context.Context, sourceURL string) (lastModified time.Time, err error) {
	ctx, span := startSourceSpan(ctx, http.MethodHead)
	defer func() { finishSourceSpan(span, err, 0) }()
	response, err := s.do(ctx, http.MethodHead, sourceURL)
	if err != nil {
		return time.Time{}, err
	}
	defer response.Body.Close()
	span.SetAttributes(attribute.Int(sourceStatusCodeAttr, response.StatusCode))
	return parseLastModified(response.Header.Get("Last-Modified")), nil
}

func (s *HTTPSource) Get(ctx context.Context, sourceURL string) (data []byte, lastModified time.Time, err error) {
	ctx, span := startSourceSpan(ctx, http.MethodGet)
	defer func() { finishSourceSpan(span, err, len(data)) }()
	response, err := s.do(ctx, http.MethodGet, sourceURL)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer response.Body.Close()
	span.SetAttributes(attribute.Int(sourceStatusCodeAttr, response.StatusCode))
	data, err = io.ReadAll(io.LimitReader(response.Body, maxFeedSize+1))
	if err != nil {
		return nil, time.Time{}, wrapSourceError("source feed read failed", err)
	}
	if len(data) > maxFeedSize {
		return nil, time.Time{}, fmt.Errorf("source feed exceeds %d bytes", maxFeedSize)
	}
	return data, parseLastModified(response.Header.Get("Last-Modified")), nil
}

func startSourceSpan(ctx context.Context, method string) (context.Context, trace.Span) {
	ctx, span := otel.Tracer(sourceTracerName).Start(ctx, "source."+strings.ToLower(method))
	span.SetAttributes(attribute.String(sourceMethodAttr, method))
	return ctx, span
}

func finishSourceSpan(span trace.Span, err error, bytes int) {
	span.SetAttributes(attribute.Int(sourceBytesAttr, bytes))
	if err != nil {
		errorType := fmt.Sprintf("%T", err)
		span.SetAttributes(attribute.String(sourceErrorTypeAttr, errorType))
		span.RecordError(fmt.Errorf("source operation failed: %s", errorType))
		span.SetStatus(codes.Error, "source failure")
	}
	span.End()
}

func (s *HTTPSource) do(ctx context.Context, method, sourceURL string) (*http.Response, error) {
	if _, err := validateURL(sourceURL); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, sourceURL, nil)
	if err != nil {
		return nil, wrapSourceError("source request could not be created", err)
	}
	request.Header.Set("User-Agent", "RSS-Studio/0.1")
	request.Header.Set("Accept", "application/rss+xml, application/xml, text/xml;q=0.9, */*;q=0.1")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, wrapSourceError("source request failed", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		trace.SpanFromContext(ctx).SetAttributes(attribute.Int(sourceStatusCodeAttr, response.StatusCode))
		response.Body.Close()
		return nil, fmt.Errorf("source %s returned status %d", method, response.StatusCode)
	}
	return response, nil
}

func parseLastModified(value string) time.Time {
	parsed, _ := http.ParseTime(value)
	return parsed
}

func validateURL(value string) (*url.URL, error) {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("source URL must use http or https")
	}
	return parsed, nil
}

func safeDialContext(baseDial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return safeDialContextWithLookup(net.DefaultResolver.LookupNetIP, baseDial)
}

func safeDialContextWithLookup(lookupIP func(context.Context, string, string) ([]netip.Addr, error), baseDial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("source destination is unavailable")
		}
		ips, err := lookupIP(ctx, "ip", host)
		if err != nil {
			return nil, wrapSourceError("source DNS lookup failed", err)
		}
		var lastErr error
		for _, ip := range ips {
			if blockedIP(ip) {
				continue
			}
			connection, err := baseDial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return connection, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			if err := ctx.Err(); err != nil {
				return nil, wrapSourceError("source connection canceled", err)
			}
			return nil, fmt.Errorf("source destination is blocked")
		}
		return nil, wrapSourceError("source connection failed", lastErr)
	}
}

func blockedIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Is4In6() {
		return true
	}
	if ip.Is4() {
		ip4 := ip.As4()
		return ip4[0] == 127 ||
			ip4[0] == 10 ||
			(ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) ||
			(ip4[0] == 192 && ip4[1] == 168) ||
			(ip4[0] == 169 && ip4[1] == 254) ||
			(ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127) ||
			(ip4[0] == 169 && ip4[1] == 254 && ip4[2] == 169 && ip4[3] == 254) ||
			(ip4[0] == 0 && ip4[1] == 0 && ip4[2] == 0 && ip4[3] == 0) ||
			(ip4[0] >= 224)
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

type sourceError struct {
	message string
	cause   error
}

func (e *sourceError) Error() string {
	return e.message
}

func (e *sourceError) Unwrap() error {
	return e.cause
}

func wrapSourceError(message string, cause error) error {
	return &sourceError{message: message, cause: cause}
}
