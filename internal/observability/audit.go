package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

	auditEventAttr           = "rss_studio.audit.event"
	auditOperationAttr       = "rss_studio.operation"
	auditActorIDAttr         = "rss_studio.actor.id"
	auditTokenIDAttr         = "rss_studio.token.id"
	auditResultAttr          = "rss_studio.result"
	auditDurationAttr        = "rss_studio.duration_ms"
	auditRateRejectedAttr    = "rss_studio.rate_rejected"
	auditQuotaRejectedAttr   = "rss_studio.quota_rejected"
	auditUpstreamFailureAttr = "rss_studio.upstream_failure"
	failureStageAttr         = "rss_studio.failure.stage"
	failureTypeAttr          = "rss_studio.failure.type"
)

// Event is the safe, structured audit record shared by HTTP, application, and
// future adapters. It deliberately has no request, feed, credential, or
// idempotency-key fields.
type Event struct {
	Timestamp       time.Time `json:"timestamp"`
	Event           string    `json:"event"`
	Operation       string    `json:"operation"`
	ActorID         int64     `json:"actor_id"`
	TokenID         int64     `json:"token_id"`
	Result          string    `json:"result"`
	DurationMS      int64     `json:"duration_ms"`
	RateRejected    bool      `json:"rate_rejected"`
	QuotaRejected   bool      `json:"quota_rejected"`
	UpstreamFailure bool      `json:"upstream_failure"`
	RequestID       string    `json:"request_id,omitempty"`
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
	span.RecordError(fmt.Errorf("%s: %s", stage, errorType))
	span.SetStatus(codes.Error, stage)
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
