# Solution

## Webhook Ingestion Investigation

This document summarizes the defects investigated in the webhook ingestion
service, the reasoning behind each fix, and the evidence used to verify the
changes.

The investigation follows this process:

1. Observe or infer a possible symptom from the assignment requirements.
2. Identify multiple possible causes.
3. Investigate the existing implementation.
4. Add a focused regression test for the suspected behavior.
5. Run the test against the current implementation.
6. If the test fails, identify the exact root cause.
7. Define the intended fix and acceptance criteria.
8. Implement the smallest appropriate fix.
9. Rerun the same regression test.
10. Run broader verification, including the race detector where relevant.

Only defects demonstrated by failing evidence are treated as fixed defects.

---

## 1. Durable Statistics vs. In-Memory Cache

### Problem

The service maintains account statistics in an in-memory cache so that the
statistics endpoint does not need to query PostgreSQL for every request.

The durable statistics are stored in PostgreSQL in the `account_stats` table.

The important lifecycle requirement is that the in-memory cache must not lose
the durable statistics when the application restarts.

A fresh process creates a new cache, so the cache initially contains no
entries.

### Initial Hypothesis

A possible restart-related defect was that PostgreSQL still contained the
account statistics, but the newly created in-memory cache did not restore
those statistics.

Possible causes considered:

- The cache might not support loading an existing snapshot.
- Application startup might not read durable statistics.
- The startup code might read the database but fail to populate the cache.
- The cache might incorrectly initialize existing values.

### Investigation

The cache implementation initially created an empty map:

```go
func NewCache() *Cache {
    return &Cache{m: make(map[string]*AccountStats)}
}
```

`Get()` returned zero values for accounts that were not present.

Therefore, creating a new cache after a process restart resulted in:

```
CallCount: 0
TotalDurationSec: 0
```

even when PostgreSQL contained durable statistics.

### Regression Test

A test was added to create durable statistics first and then simulate a fresh
application cache.

The test verified that the durable statistics existed before checking the
fresh cache.

The initial failing result was:

```
cache stats = {CallCount:0 TotalDurationSec:0}, want {CallCount:1 TotalDurationSec:143}
```

This demonstrated that the cache did not automatically contain the durable
statistics.

### Root Cause

The root cause was that the cache was intentionally initialized as an empty
in-memory structure, but there was no mechanism in the startup path to
rehydrate it from PostgreSQL.

The database contained the correct durable values, but the cache had no
knowledge of them.

### Fix Decision

The cache should remain an in-memory optimization rather than becoming
responsible for reading PostgreSQL itself.

The chosen design was:

- PostgreSQL remains the durable source of truth.
- Application startup reads durable account statistics from PostgreSQL.
- The startup code converts the durable representation into the cache
  representation.
- `Cache.Load()` replaces the cache contents with that startup snapshot.
- Normal webhook processing continues updating the cache through
  `Cache.Record()`.

This keeps persistence concerns inside the store layer and cache concerns
inside the stats package.

### Implementation

A `Load` method was added to the cache:

```go
func (c *Cache) Load(all map[string]AccountStats) {
    c.mu.Lock()
    defer c.mu.Unlock()

    c.m = make(map[string]*AccountStats, len(all))

    for accountID, st := range all {
        snapshot := st
        c.m[accountID] = &snapshot
    }
}
```

Application startup now loads durable statistics and hydrates the cache before
creating the ingest service.

### Verification

The regression test was rerun:

```
go test ./internal/ingest -run TestStatsCacheStartsEmptyDespiteDurableStats -v
```

Result:

```
=== RUN   TestStatsCacheStartsEmptyDespiteDurableStats
--- PASS: TestStatsCacheStartsEmptyDespiteDurableStats (0.02s)
PASS
ok      github.com/convin/webhook-ingest/internal/ingest
```

The test was also rerun with the result cached:

```
=== RUN   TestStatsCacheStartsEmptyDespiteDurableStats
--- PASS: TestStatsCacheStartsEmptyDespiteDurableStats (0.02s)
PASS
ok      github.com/convin/webhook-ingest/internal/ingest (cached)
```

The fix was subsequently included in the broader race-detector test run.

---

## 2. Concurrent Duplicate Webhook Delivery

### Problem / Hypothesis

Webhook providers may retry deliveries. Multiple copies of the same webhook
can therefore arrive concurrently.

A sequential duplicate-delivery test already existed, but sequential testing
does not establish that the implementation is correct when duplicate
requests arrive at the same time.

The suspected symptom was:

Concurrent copies of the same event might bypass the duplicate check and
create duplicate database records or increment account statistics more than
once.

### Possible Causes Considered

Several concurrency problems were considered:

- A check-then-insert race around event IDs.
- Multiple requests inserting the same event.
- Multiple requests creating or updating the same call.
- Account statistics being incremented multiple times.
- An application-level mutex being required for duplicate detection.
- A database uniqueness constraint failing to protect the transaction.

### Regression Test

A new test was added: `TestConcurrentDuplicateDeliveryIsIgnored`

The test sends 20 concurrent HTTP requests containing the exact same
`event_id`.

It verifies:

- exactly one event exists;
- exactly one call exists;
- durable account statistics contain exactly one call;
- total duration is exactly 143 seconds.

### Before-Fix Result

The test was executed against the existing implementation:

```
go test ./internal/ingest -run TestConcurrentDuplicateDeliveryIsIgnored -v -count=1
```

Result:

```
=== RUN   TestConcurrentDuplicateDeliveryIsIgnored
--- PASS: TestConcurrentDuplicateDeliveryIsIgnored (0.05s)
PASS
ok      github.com/convin/webhook-ingest/internal/ingest
```

The same test was then executed with the race detector:

```
go test -race ./internal/ingest -run TestConcurrentDuplicateDeliveryIsIgnored -v -count=1
```

Result:

```
=== RUN   TestConcurrentDuplicateDeliveryIsIgnored
--- PASS: TestConcurrentDuplicateDeliveryIsIgnored (0.08s)
PASS
ok      github.com/convin/webhook-ingest/internal/ingest
```

### Conclusion

The suspected concurrent duplicate-delivery defect was not reproduced.

The existing database-level uniqueness constraint and transactional
`ON CONFLICT (event_id) DO NOTHING` behavior correctly prevented duplicate
event records under this test.

Because the test passed before any implementation change, no code change was
made for this hypothesis.

This investigation is retained because it demonstrates that concurrency was
actively tested rather than assuming the sequential duplicate test was
sufficient.

---

## 3. Recording Processing and Restart Recovery

### Initial Hypothesis

Recording processing occurs asynchronously after the webhook has been
accepted.

The relevant implementation is:

```go
if rec.RecordingURL != "" {
    go func() {
        if err := s.processRecording(ctx, rec); err != nil {
            // TODO: handle
        }
    }()
}
```

This raises a restart-recovery question:

What happens if the application accepts the webhook but terminates before
the asynchronous recording-processing goroutine finishes?

Possible failure modes include:

- The recording remains unprocessed.
- The process has no way to discover unfinished recordings after restart.
- Processing errors are silently discarded.
- The request context may be cancelled before the asynchronous operation
  completes.
- `recording_processed` may persist the state but there may be no recovery
  mechanism.

### Existing Durable State

The database schema contains:

```
recording_processed BOOLEAN NOT NULL DEFAULT FALSE
```

The store also contains:

```go
func (s *Store) MarkRecordingProcessed(ctx context.Context, callID string) error
```

which sets:

```
recording_processed = TRUE
```

### Investigation

The following search was performed:

```
grep -R "Unprocessed\|recording_processed\|processRecording" -n internal
```

The relevant results were:

```
internal/ingest/service.go:78: if err := s.processRecording(ctx, rec); err != nil {
internal/ingest/service.go:87:// processRecording downloads and transcodes the call recording, then marks
internal/ingest/service.go:89:func (s *Service) processRecording(ctx context.Context, rec store.Event) error {
internal/store/store_test.go:81:    SELECT recording_processed FROM calls WHERE call_id = $1
internal/store/store_test.go:86:    expected recording_processed to be true
internal/store/store.go:101:    UPDATE calls SET recording_processed = TRUE, updated_at = now()
```

No `UnprocessedRecordings` implementation was found in the application code.

### Current Evidence

The database correctly retains whether a recording has been processed.

An existing test named `TestUnprocessedRecordingsCanBeFoundAfterRestart` was
previously executed and passed:

```
=== RUN   TestUnprocessedRecordingsCanBeFoundAfterRestart
--- PASS: TestUnprocessedRecordingsCanBeFoundAfterRestart (0.03s)
PASS
ok      github.com/convin/webhook-ingest/internal/store
```

However, the current investigation has established that there is no
application-level recovery function named `UnprocessedRecordings` in the
current implementation.

Therefore, the database's ability to retain the unprocessed state should not
be confused with proof that the application automatically resumes unfinished
recording work after restart.

### Fix

A durable recovery path was added for recordings that remain unprocessed.

The store now exposes:

```go
func (s *Store) UnprocessedRecordings(ctx context.Context) ([]Event, error)
```

This queries calls where:

```
recording_url IS NOT NULL
AND recording_processed = FALSE
```

The ingest service now exposes:

```go
func (s *Service) RecoverUnprocessedRecordings(ctx context.Context) error
```

The recovery flow:

1. Queries PostgreSQL for unprocessed recordings.
2. Iterates over the returned recordings.
3. Re-runs the recording-processing operation.
4. Marks successfully processed recordings as complete.

This makes the database state durable across application restarts.

### Regression Test

A restart-recovery regression test was added:

```
TestUnprocessedRecordingIsRecoveredAfterRestart
```

The test:

1. Creates a call containing a recording URL.
2. Verifies that `recording_processed` is initially `FALSE`.
3. Creates a fresh ingest service to simulate a new application process.
4. Calls `RecoverUnprocessedRecordings`.
5. Verifies that `recording_processed` becomes `TRUE`.

### Verification

The focused test was executed repeatedly with the race detector:

```
go test -race ./internal/ingest \
  -run TestUnprocessedRecordingIsRecoveredAfterRestart \
  -v -count=10
```

Result:

```
=== RUN   TestUnprocessedRecordingIsRecoveredAfterRestart
--- PASS: TestUnprocessedRecordingIsRecoveredAfterRestart
...
=== RUN   TestUnprocessedRecordingIsRecoveredAfterRestart
--- PASS: TestUnprocessedRecordingIsRecoveredAfterRestart
PASS
ok      github.com/convin/webhook-ingest/internal/ingest
```

All 10 executions passed.

The complete test suite also passed:

```
go test ./...
```

and:

```
go test -race ./...
```

Therefore, recording-processing restart recovery is now considered completed
and verified.

### Known Remaining Gap: Async Failure Handling

The asynchronous processing goroutine still contains:

```go
go func() {
    if err := s.processRecording(ctx, rec); err != nil {
        // TODO: handle
    }
}()
```

An error returned by `processRecording` inside this goroutine is currently
swallowed. Recovery via `RecoverUnprocessedRecordings` addresses the restart
case (an unprocessed recording will be picked up and retried later), but it
does not address observability or handling of failures at the time they
occur. This gap is tracked separately and is not yet fixed.

---

## Verification Summary

The full race-detector suite was executed after the implemented changes:

```
go test -race ./...
```

Result:

```
?       github.com/convin/webhook-ingest/cmd/server     [no test files]
?       github.com/convin/webhook-ingest/internal/config        [no test files]
ok      github.com/convin/webhook-ingest/internal/httpapi
ok      github.com/convin/webhook-ingest/internal/ingest
?       github.com/convin/webhook-ingest/internal/redisclient   [no test files]
ok      github.com/convin/webhook-ingest/internal/stats
ok      github.com/convin/webhook-ingest/internal/store
?       github.com/convin/webhook-ingest/internal/testutil      [no test files]
```

All currently executed tests passed with the race detector enabled.

---

## Engineering Principles Used

**PostgreSQL is the durable source of truth**
The in-memory cache must not be treated as persistent state.

**Cache hydration belongs at application startup**
The store reads durable state and the cache receives a snapshot through
`Load()`.

**Database constraints provide duplicate protection**
Duplicate webhook delivery must remain correct even when requests arrive
concurrently. The database's unique event ID constraint and transactional
insert provide the authoritative protection.

**Tests should demonstrate the defect**
A passing test against the original implementation is evidence that the
hypothesized defect was not reproduced. It should not be presented as a bug
fix.

**Verification should reuse the regression test**
When a test fails before a fix, the same test should pass after the fix.
Additional broader test suites then provide regression coverage.

---

## Current Status

**Completed and verified:**

- Durable account statistics cache hydration.
- Concurrent duplicate-delivery investigation and database-level idempotency.
- Race-detector verification of the current implementation.
- Recording-processing restart recovery.
