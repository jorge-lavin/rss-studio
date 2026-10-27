package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"rss-studio/internal/domain"
	"rss-studio/internal/policy"
)

type eventRecorder struct {
	events []Event
}

type classifiedFailureError struct{}

func (classifiedFailureError) Error() string         { return "private parser detail" }
func (classifiedFailureError) FailureClass() string  { return "invalid_xml" }
func (classifiedFailureError) FailureReason() string { return "unexpected_eof" }
func (classifiedFailureError) FailureLine() int      { return 17 }

func (r *eventRecorder) Record(event Event) {
	r.events = append(r.events, event)
}

func TestRecordAttributesEventsToAuthenticatedIdentityWithoutSensitiveFields(t *testing.T) {
	recorder := &eventRecorder{}
	ctx := policy.WithIdentity(context.Background(), policy.Identity{
		User:    domain.User{ID: 42, Username: "account-name"},
		TokenID: 7,
	})
	ctx = WithRecorder(ctx, recorder)

	Record(ctx, Event{
		Event:         EventMutation,
		Operation:     "feeds.create",
		ActorID:       999,
		TokenID:       999,
		Result:        "success",
		DurationMS:    12,
		RateRejected:  false,
		QuotaRejected: false,
	})

	if len(recorder.events) != 1 {
		t.Fatalf("got %d audit events, want one", len(recorder.events))
	}
	event := recorder.events[0]
	if event.ActorID != 42 || event.TokenID != 7 || event.Operation != "feeds.create" || event.Result != "success" || event.DurationMS != 12 {
		t.Fatalf("unexpected attributed event: %#v", event)
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{"account-name", "Bearer", "password", "Idempotency-Key", "https://example.test/rss"} {
		if strings.Contains(string(data), sensitive) {
			t.Fatalf("audit event contains sensitive field %q: %s", sensitive, data)
		}
	}
	if event.Timestamp.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("event timestamp was not recorded: %v", event.Timestamp)
	}
}

func TestActorIDsUsesOwnerFallbackWithoutIdentity(t *testing.T) {
	actorID, tokenID := ActorIDs(context.Background(), 42)
	if actorID != 42 || tokenID != 0 {
		t.Fatalf("unexpected fallback actor IDs: actor=%d token=%d", actorID, tokenID)
	}
}

func TestRecordFailureAddsSafeStageAndType(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := trace.NewTracerProvider(trace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})

	ctx, span := otel.Tracer("test").Start(context.Background(), "feed operation")
	RecordFailure(ctx, "source.fetch", fmt.Errorf("request contained https://private.example/rss"))
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected one span, got %d", len(spans))
	}
	spanData := spans[0]
	var stage, errorType string
	for _, attribute := range spanData.Attributes {
		switch attribute.Key {
		case failureStageAttr:
			stage = attribute.Value.AsString()
		case failureTypeAttr:
			errorType = attribute.Value.AsString()
		}
	}
	if stage != "source.fetch" || errorType != "_errors.errorString" {
		t.Fatalf("unexpected failure attributes: stage=%q type=%q", stage, errorType)
	}
	if len(spanData.Events) != 1 {
		t.Fatalf("expected one failure event, got %#v", spanData.Events)
	}
	for _, attribute := range spanData.Events[0].Attributes {
		if attribute.Key == "exception.message" && strings.Contains(attribute.Value.AsString(), "private.example") {
			t.Fatalf("failure event exposed request data: %#v", spanData.Events)
		}
	}
}

func TestRecordFailureAddsStableClass(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := trace.NewTracerProvider(trace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})

	ctx, span := otel.Tracer("test").Start(context.Background(), "feed operation")
	RecordFailure(ctx, "rss.preview", classifiedFailureError{})
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected one span, got %d", len(spans))
	}
	var class string
	var reason string
	var line int
	for _, attribute := range spans[0].Attributes {
		switch attribute.Key {
		case failureClassAttr:
			class = attribute.Value.AsString()
		case failureReasonAttr:
			reason = attribute.Value.AsString()
		case failureLineAttr:
			line = int(attribute.Value.AsInt64())
		}
	}
	if class != "invalid_xml" {
		t.Fatalf("failure class = %q", class)
	}
	if reason != "unexpected_eof" || line != 17 {
		t.Fatalf("failure details = %q at line %d", reason, line)
	}
}
