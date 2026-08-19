# Webhook Ingestion Solution

## Concurrent Duplicate Delivery Investigation

### Hypothesis

Concurrent webhook deliveries containing the same `event_id` might bypass the idempotency mechanism and result in duplicate events, calls, or account statistics.

### Investigation

A regression test was added:

`TestConcurrentDuplicateDeliveryIsIgnored`

The test sends 20 concurrent HTTP requests containing the same `event_id`, `call_id`, and `account_id`.

The test verifies that:

- exactly one event is stored;
- exactly one call record exists;
- durable account statistics contain exactly one call;
- total duration is counted exactly once.

### Verification

The targeted test was executed with:

```bash
go test ./internal/ingest -run TestConcurrentDuplicateDeliveryIsIgnored -v -count=1


=== RUN   TestConcurrentDuplicateDeliveryIsIgnored
--- PASS: TestConcurrentDuplicateDeliveryIsIgnored
PASS

The same test was also executed with the race detector:
go test -race ./internal/ingest -run TestConcurrentDuplicateDeliveryIsIgnored -v -count=1
Result:
=== RUN   TestConcurrentDuplicateDeliveryIsIgnored
--- PASS: TestConcurrentDuplicateDeliveryIsIgnored
PASS
Conclusion
The suspected concurrency/idempotency defect was not reproduced.
The existing implementation correctly handled the tested concurrent duplicate deliveries.
Therefore:
no production implementation change was made for this hypothesis;
the regression test was retained as additional protection;
this investigation is not claimed as a fixed defect.



---

# Step 2 — Update `SOLUTION.md`

Don't claim a fix yet.

Add:

```markdown
## Recording Recovery Investigation

The incident report states that recordings in flight appear to disappear after deployment.

The first investigation test, `TestUnprocessedRecordingsCanBeFoundAfterRestart`, established that the unfinished recording state is durable in PostgreSQL.

A call with:

```text
recording_processed = FALSE

---

# Step 3 — Commit this investigation

Now this is a legitimate investigation checkpoint.

Run:

```bash
gofmt -w internal/store/store_test.go

git diff --check

git status

git diff --stat