package source

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxFeedSize = 25 << 20

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

func (s *HTTPSource) Head(ctx context.Context, sourceURL string) (time.Time, error) {
	response, err := s.do(ctx, http.MethodHead, sourceURL)
	if err != nil {
		return time.Time{}, err
	}
	defer response.Body.Close()
	return parseLastModified(response.Header.Get("Last-Modified")), nil
}

func (s *HTTPSource) Get(ctx context.Context, sourceURL string) ([]byte, time.Time, error) {
	response, err := s.do(ctx, http.MethodGet, sourceURL)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxFeedSize+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(data) > maxFeedSize {
		return nil, time.Time{}, fmt.Errorf("source feed exceeds %d bytes", maxFeedSize)
	}
	return data, parseLastModified(response.Header.Get("Last-Modified")), nil
}

func (s *HTTPSource) do(ctx context.Context, method, sourceURL string) (*http.Response, error) {
	if _, err := validateURL(sourceURL); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "RSS-Studio/0.1")
	request.Header.Set("Accept", "application/rss+xml, application/xml, text/xml;q=0.9, */*;q=0.1")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, fmt.Errorf("source %s returned %s", method, response.Status)
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
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			if blockedIP(ip) {
				lastErr = fmt.Errorf("blocked destination %s", ip)
				continue
			}
			connection, err := baseDial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return connection, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no usable destination for %s", host)
		}
		return nil, lastErr
	}
}

func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || strings.EqualFold(ip.String(), "169.254.169.254")
}
