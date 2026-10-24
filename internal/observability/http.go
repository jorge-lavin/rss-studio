package observability

import (
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName    = "rss-studio/http"
	slowRequestThreshold   = time.Second
	requestDurationAttr    = "rss_studio.request.duration_ms"
	httpRequestMethodAttr  = "http.request.method"
	httpResponseStatusAttr = "http.response.status_code"
	httpRouteAttr          = "http.route"
)

// HTTPHandler records an OpenTelemetry server span and logs request outcomes.
// Paths are represented by matched routes to avoid putting feed slugs in telemetry.
func HTTPHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(request.Context(), propagation.HeaderCarrier(request.Header))
		ctx, span := otel.Tracer(instrumentationName).Start(ctx, request.Method)
		request = request.WithContext(ctx)
		started := time.Now()
		response := &statusWriter{ResponseWriter: writer}
		panicked := false

		defer func() {
			if recovered := recover(); recovered != nil {
				panicked = true
				panicError := fmt.Errorf("panic: %T", recovered)
				log.Printf("panic method=%s route=%s type=%T\n%s", request.Method, requestRoute(request), recovered, debug.Stack())
				span.RecordError(panicError, trace.WithStackTrace(true))
				span.SetStatus(codes.Error, "panic")
				response.WriteHeader(http.StatusInternalServerError)
			}

			duration := time.Since(started)
			status := response.Status()
			route := requestRoute(request)
			span.SetName(request.Method + " " + route)
			span.SetAttributes(
				attribute.String(httpRequestMethodAttr, request.Method),
				attribute.Int(httpResponseStatusAttr, status),
				attribute.String(httpRouteAttr, route),
				attribute.Int64(requestDurationAttr, duration.Milliseconds()),
			)
			if status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(status))
				if !panicked {
					span.RecordError(fmt.Errorf("HTTP status %d", status))
				}
			}

			if duration >= slowRequestThreshold {
				log.Printf("slow request method=%s route=%s status=%d duration=%s", request.Method, route, status, duration.Round(time.Millisecond))
			} else {
				log.Printf("request method=%s route=%s status=%d duration=%s", request.Method, route, status, duration.Round(time.Millisecond))
			}
			span.End()
		}()

		next.ServeHTTP(response, request)
	})
}

func requestRoute(request *http.Request) string {
	route := request.Pattern
	if method, pattern, found := strings.Cut(route, " "); found && method == request.Method {
		route = pattern
	}
	if route != "" {
		return route
	}
	return "unmatched"
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (writer *statusWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *statusWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(data)
}

func (writer *statusWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *statusWriter) Status() int {
	if writer.status == 0 {
		return http.StatusOK
	}
	return writer.status
}
