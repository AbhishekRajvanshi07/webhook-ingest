Hypothesis
→ Concurrent duplicate deliveries might bypass idempotency and create duplicate events/calls/stats.

Test added
→ TestConcurrentDuplicateDeliveryIsIgnored
→ 20 concurrent requests with the same event_id.

Before-fix result
→ PASS

Race detector result
→ PASS

Conclusion
→ Defect NOT reproduced.
→ No implementation change made.
→ Do not claim this as a fixed defect.

---

## Recording Processing After Application Restart

### Hypothesis

A webhook containing a recording URL is accepted and the recording is processed asynchronously.

If the application stops before the asynchronous processing completes, the call remains in PostgreSQL with `recording_processed = FALSE`.

The hypothesis is that the application may not recover these unfinished recordings after restart, which could leave them permanently unprocessed.

### Initial Evidence

The database schema contains durable `recording_processed` state.

The service starts recording processing asynchronously from `Ingest()`.

The current service startup path does not yet appear to explicitly recover calls whose recordings remain unprocessed.

### Unknowns

At this stage the root cause is not assumed.

We need to determine through testing whether:

1. an unprocessed recording can be persisted;
2. the unprocessed state survives application restart;
3. a newly created service can discover the unfinished recording;
4. the recording is eventually marked processed after restart.

### Investigation Plan

Add a test that simulates an application restart:

1. Persist a call with a recording URL.
2. Leave `recording_processed` as `FALSE`.
3. Create a fresh store/service representing a new application process.
4. Determine whether the unfinished recording can be discovered.
5. Run the test before changing production code.

The test result will determine whether the suspected restart/recovery defect actually exists.

Investigation test
→ Initial test execution failed during compilation.

Failure
→ Go import cycle in store_test.go.

Cause
→ Test file package declaration/import relationship was incorrect.

Action
→ Restore store_test.go to external package `store_test`.

Expected result
→ Test compiles and exercises the intended restart-persistence hypothesis.

## Recording Processing After Application Restart

### Hypothesis

A webhook containing a recording URL is accepted and the recording is processed asynchronously.

If the application stops before the asynchronous processing completes, the call remains in PostgreSQL with `recording_processed = FALSE`.

The hypothesis is that the application may not recover these unfinished recordings after restart, which could leave them permanently unprocessed.

### Initial Evidence

The database schema contains durable `recording_processed` state.

The service starts recording processing asynchronously from `Ingest()`.

The current service startup path does not explicitly recover calls whose recordings remain unprocessed.

### Investigation Test 1 — Durable Unprocessed Recording

A test was added:

`TestUnprocessedRecordingsCanBeFoundAfterRestart`

The test:

1. Creates a call with a recording URL.
2. Persists the call without marking its recording as processed.
3. Verifies that `recording_processed` remains `FALSE`.
4. Queries PostgreSQL for unfinished recordings using the durable state.

### First Attempt

The first test execution could not reach the application logic because PostgreSQL was not running.

Observed result:

```text
connect to postgres ... localhost:5432 ... connection refused