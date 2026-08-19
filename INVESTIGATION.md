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