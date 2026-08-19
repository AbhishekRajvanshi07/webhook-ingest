package ingest_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/convin/webhook-ingest/internal/ingest"
	"github.com/convin/webhook-ingest/internal/stats"
	"github.com/convin/webhook-ingest/internal/store"
	"github.com/convin/webhook-ingest/internal/testutil"
)

// eventJSON builds a well-formed call-completion payload.
func eventJSON(eventID, callID, accountID string) string {
	return fmt.Sprintf(`{
	  "event_id":      %q,
	  "call_id":       %q,
	  "account_id":    %q,
	  "status":        "completed",
	  "duration_sec":  143,
	  "recording_url": "https://recordings.example.com/%s.wav",
	  "occurred_at":   "2026-08-13T09:12:00Z"
	}`, eventID, callID, accountID, callID)
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()

	resp, err := http.Post(
		url,
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	t.Cleanup(func() {
		_ = resp.Body.Close()
	})

	return resp
}

func TestWebhookStoresEventAndCall(t *testing.T) {
	srv, st := testutil.NewServer(t)
	eventID, callID, accountID := testutil.IDs(t, st)
	ctx := context.Background()

	body := eventJSON(eventID, callID, accountID)

	if resp := post(t, srv.URL+"/webhooks/calls", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	exists, err := st.EventExists(ctx, eventID)
	if err != nil {
		t.Fatalf("EventExists: %v", err)
	}

	if !exists {
		t.Fatal("expected the event to be stored")
	}

	var gotAccount string

	row := st.Pool().QueryRow(
		ctx,
		`SELECT account_id FROM calls WHERE call_id = $1`,
		callID,
	)

	if err := row.Scan(&gotAccount); err != nil {
		t.Fatalf(
			"expected a call record for %s: %v",
			callID,
			err,
		)
	}

	if gotAccount != accountID {
		t.Fatalf(
			"call belongs to %q, want %q",
			gotAccount,
			accountID,
		)
	}
}

func TestDuplicateDeliveryIsIgnored(t *testing.T) {
	srv, st := testutil.NewServer(t)
	eventID, callID, accountID := testutil.IDs(t, st)
	ctx := context.Background()

	body := eventJSON(eventID, callID, accountID)

	for i := 0; i < 3; i++ {
		if resp := post(
			t,
			srv.URL+"/webhooks/calls",
			body,
		); resp.StatusCode != http.StatusOK {
			t.Fatalf(
				"delivery %d: got %d, want 200",
				i,
				resp.StatusCode,
			)
		}
	}

	var n int

	row := st.Pool().QueryRow(
		ctx,
		`SELECT count(*) FROM events WHERE event_id = $1`,
		eventID,
	)

	if err := row.Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if n != 1 {
		t.Fatalf(
			"stored %d copies of %s, want 1",
			n,
			eventID,
		)
	}
}

func TestConcurrentDuplicateDeliveryIsIgnored(t *testing.T) {
	srv, st := testutil.NewServer(t)
	eventID, callID, accountID := testutil.IDs(t, st)
	ctx := context.Background()

	body := eventJSON(eventID, callID, accountID)

	const requests = 20

	var wg sync.WaitGroup
	errs := make(chan error, requests)

	for i := 0; i < requests; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			resp, err := http.Post(
				srv.URL+"/webhooks/calls",
				"application/json",
				strings.NewReader(body),
			)

			if err != nil {
				errs <- err
				return
			}

			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				responseBody, _ := io.ReadAll(resp.Body)

				errs <- fmt.Errorf(
					"got status %d, body: %s",
					resp.StatusCode,
					responseBody,
				)
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	var eventCount int

	err := st.Pool().QueryRow(
		ctx,
		`SELECT count(*) FROM events WHERE event_id = $1`,
		eventID,
	).Scan(&eventCount)

	if err != nil {
		t.Fatalf("count events: %v", err)
	}

	if eventCount != 1 {
		t.Fatalf(
			"stored %d copies of event, want 1",
			eventCount,
		)
	}

	var callCount int

	err = st.Pool().QueryRow(
		ctx,
		`SELECT count(*) FROM calls WHERE call_id = $1`,
		callID,
	).Scan(&callCount)

	if err != nil {
		t.Fatalf("count calls: %v", err)
	}

	if callCount != 1 {
		t.Fatalf(
			"stored %d call records, want 1",
			callCount,
		)
	}

	durable, err := st.AccountStats(ctx, accountID)
	if err != nil {
		t.Fatalf("AccountStats: %v", err)
	}

	if durable.CallCount != 1 ||
		durable.TotalDurationSec != 143 {
		t.Fatalf(
			"durable stats = %+v, want CallCount=1 TotalDurationSec=143",
			durable,
		)
	}
}

func TestUnprocessedRecordingIsRecoveredAfterRestart(t *testing.T) {
	st := testutil.NewStore(t)
	eventID, callID, accountID := testutil.IDs(t, st)
	ctx := context.Background()

	evt := store.Event{
		EventID:      eventID,
		CallID:       callID,
		AccountID:    accountID,
		Status:       "completed",
		DurationSec:  10,
		RecordingURL: "https://example.com/recording.wav",
		Payload:      []byte(`{}`),
	}

	if err := st.UpsertCall(ctx, evt); err != nil {
		t.Fatalf("UpsertCall: %v", err)
	}

	var processed bool

	err := st.Pool().QueryRow(
		ctx,
		`SELECT recording_processed
		 FROM calls
		 WHERE call_id = $1`,
		callID,
	).Scan(&processed)

	if err != nil {
		t.Fatalf("check recording state: %v", err)
	}

	if processed {
		t.Fatal("expected recording to remain unprocessed")
	}

	// Simulate a fresh application process.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc := ingest.New(
		st,
		stats.NewCache(),
		nil,
		log,
	)

	// This is the behavior we expect after a restart.
	if err := svc.RecoverUnprocessedRecordings(ctx); err != nil {
		t.Fatalf(
			"RecoverUnprocessedRecordings: %v",
			err,
		)
	}

	err = st.Pool().QueryRow(
		ctx,
		`SELECT recording_processed
		 FROM calls
		 WHERE call_id = $1`,
		callID,
	).Scan(&processed)

	if err != nil {
		t.Fatalf("check recovered recording: %v", err)
	}

	if !processed {
		t.Fatal("expected recording to be processed after recovery")
	}
}
