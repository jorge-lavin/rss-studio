package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"rss-studio/internal/policy"
)

const (
	EventOperation = "operation"
	EventMutation  = "mutation"
	EventPolicy    = "policy"
	EventRateLimit = "rate_limit"
	EventQuota     = "quota"
	EventUpstream  = "upstream"

	auditEventAttr            = "rss_studio.audit.event"
	auditOperationAttr        = "rss_studio.operation"
	auditActorIDAttr          = "rss_studio.actor.id"
	auditTokenIDAttr          = "rss_studio.token.id"
	auditResultAttr           = "rss_studio.result"
	auditDurationAttr         = "rss_studio.duration_ms"
	auditRateRejectedAttr     = "rss_studio.rate_rejected"
	auditQuotaRejectedAttr    = "rss_studio.quota_rejected"
	auditUpstreamFailureAttr  = "rss_studio.upstream_failure"
	auditUpstreamIdentityAttr = "rss_studio.upstream.identity"
	auditUpstreamStatusAttr   = "rss_studio.upstream.status_code"
	auditUpstreamClassAttr    = "rss_studio.upstream.error_class"
	auditUpstreamDurationAttr = "rss_studio.upstream.duration_ms"
	failureStageAttr          = "rss_studio.failure.stage"
	failureTypeAttr           = "rss_studio.failure.type"
	failureClassAttr          = "rss_studio.failure.class"
)

// Event is the safe, structured audit record shared by HTTP, application, and
// future adapters. It deliberately has no request, feed, credential, or
// idempotency-key fields.
type Event struct {
	Timestamp          time.Time `json:"timestamp"`
	Event              string    `json:"event"`
	Operation          string    `json:"operation"`
	ActorID            int64     `json:"actor_id"`
	TokenID            int64     `json:"token_id"`
	Result             string    `json:"result"`
	DurationMS         int64     `json:"duration_ms"`
	RateRejected       bool      `json:"rate_rejected"`
	QuotaRejected      bool      `json:"quota_rejected"`
	UpstreamFailure    bool      `json:"upstream_failure"`
	UpstreamIdentity   string    `json:"upstream_identity,omitempty"`
	UpstreamStatusCode int       `json:"upstream_status_code,omitempty"`
	UpstreamErrorClass string    `json:"upstream_error_class,omitempty"`
	RequestID          string    `json:"request_id,omitempty"`
}

// Recorder receives safe audit events. Implementations must not add request
// data to records.
type Recorder interface {
	Record(Event)
}

type recorderContextKey struct{}

// WithRecorder overrides default audit logging for one context. It is useful
// for deterministic tests and embedding RSS Studio in another process.
func WithRecorder(ctx context.Context, recorder Recorder) context.Context {
	return context.WithValue(ctx, recorderContextKey{}, recorder)
}

type operationContextKey struct{}

// WithOperation carries stable operation identity into lower-level adapters.
func WithOperation(ctx context.Context, operation string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, operationContextKey{}, operation)
}

// Operation returns stable operation identity for lower-level telemetry.
func Operation(ctx context.Context) string {
	if ctx != nil {
		if operation, ok := ctx.Value(operationContextKey{}).(string); ok && strings.TrimSpace(operation) != "" {
			return safeField(operation)
		}
	}
	return "upstream.fetch"
}

func recorderFromContext(ctx context.Context) Recorder {
	if ctx == nil {
		return nil
	}
	recorder, _ := ctx.Value(recorderContextKey{}).(Recorder)
	return recorder
}

// Record emits one safe audit event, adds its fields to the active span, and
// falls back to the process logger when no context recorder is configured.
func Record(ctx context.Context, event Event) {
	if ctx == nil {
		ctx = context.Background()
	}
	event.ActorID, event.TokenID = ActorIDs(ctx, event.ActorID)
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if event.RequestID == "" {
		event.RequestID, _ = ctx.Value(requestIDContextKey{}).(string)
	}
	event.Event = safeField(event.Event)
	event.Operation = safeField(event.Operation)
	event.Result = safeField(event.Result)
	event.UpstreamIdentity = SanitizeUpstreamIdentity(event.UpstreamIdentity)
	event.UpstreamErrorClass = safeField(event.UpstreamErrorClass)
	event.RequestID = safeField(event.RequestID)

	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetAttributes(
			attribute.String(auditEventAttr, event.Event),
			attribute.String(auditOperationAttr, event.Operation),
			attribute.Int64(auditActorIDAttr, event.ActorID),
			attribute.Int64(auditTokenIDAttr, event.TokenID),
			attribute.String(auditResultAttr, event.Result),
			attribute.Int64(auditDurationAttr, event.DurationMS),
			attribute.Bool(auditRateRejectedAttr, event.RateRejected),
			attribute.Bool(auditQuotaRejectedAttr, event.QuotaRejected),
			attribute.Bool(auditUpstreamFailureAttr, event.UpstreamFailure),
		)
		if event.Event == EventUpstream {
			span.SetAttributes(
				attribute.String(auditUpstreamIdentityAttr, event.UpstreamIdentity),
				attribute.Int(auditUpstreamStatusAttr, event.UpstreamStatusCode),
				attribute.String(auditUpstreamClassAttr, event.UpstreamErrorClass),
				attribute.Int64(auditUpstreamDurationAttr, event.DurationMS),
			)
		}
	}
	if recorder := recorderFromContext(ctx); recorder != nil {
		recorder.Record(event)
		return
	}
	defaultRecorder.Record(event)
}

type logRecorder struct {
	logger *log.Logger
}

// NewLogRecorder returns a recorder that writes one JSON event per log line.
// Event schema is intentionally closed to keep sensitive fields out of logs.
func NewLogRecorder(logger *log.Logger) Recorder {
	if logger == nil {
		logger = log.Default()
	}
	return logRecorder{logger: logger}
}

var defaultRecorder = NewLogRecorder(nil)

func (r logRecorder) Record(event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	r.logger.Printf("audit %s", data)
}

// SetOperationAttributes adds stable operation identity to an active request
// span without adding the request URL or body.
func SetOperationAttributes(ctx context.Context, operation string, actorID, tokenID int64) {
	if ctx == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(
		attribute.String(auditOperationAttr, safeField(operation)),
		attribute.Int64(auditActorIDAttr, actorID),
		attribute.Int64(auditTokenIDAttr, tokenID),
	)
}

// SanitizeUpstreamIdentity keeps only scheme, hostname, and optional port.
// Paths, queries, fragments, and credentials are intentionally discarded.
func SanitizeUpstreamIdentity(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "unknown"
	}
	host := strings.ToLower(parsed.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	identity := strings.ToLower(parsed.Scheme) + "://" + host
	if port := parsed.Port(); port != "" {
		identity += ":" + port
	}
	return identity
}

// RecordFailure adds a safe failure location and error type to the active
// span. It deliberately omits the error message because wrapped errors may
// contain source URLs or other request data.
func RecordFailure(ctx context.Context, stage string, err error) {
	if ctx == nil || err == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	stage = safeField(stage)
	errorType := safeField(fmt.Sprintf("%T", err))
	span.SetAttributes(
		attribute.String(failureStageAttr, stage),
		attribute.String(failureTypeAttr, errorType),
	)
	if class := failureClass(err); class != "" {
		span.SetAttributes(attribute.String(failureClassAttr, class))
	}
	span.RecordError(fmt.Errorf("%s: %s", stage, errorType))
	span.SetStatus(codes.Error, stage)
}

type failureClassifier interface {
	FailureClass() string
}

func failureClass(err error) string {
	var classifier failureClassifier
	if errors.As(err, &classifier) {
		return safeField(classifier.FailureClass())
	}
	return ""
}

// ActorIDs returns authenticated account and service-token identifiers. The
// fallback is used by shared application calls that receive an owner directly.
func ActorIDs(ctx context.Context, fallbackActorID int64) (actorID, tokenID int64) {
	if identity, ok := policy.IdentityFromContext(ctx); ok {
		actorID = identity.User.ID
		if actorID <= 0 {
			actorID = fallbackActorID
		}
		return actorID, identity.TokenID
	}
	return fallbackActorID, 0
}

func safeField(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		value = value[:128]
	}
	for i, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("._:-/", character) {
			continue
		}
		value = value[:i] + "_" + value[i+len(string(character)):]
	}
	return value
}
