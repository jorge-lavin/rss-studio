package observability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"rss-studio/internal/domain"
	"rss-studio/internal/policy"
)

type eventRecorder struct {
	events []Event
}

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
