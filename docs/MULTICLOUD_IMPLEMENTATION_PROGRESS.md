# Multi-cloud Billing implementation checkpoint

This is implementation evidence, not replacement normative design. The approved
target remains `CLOUD_OWNERSHIP_HANDOFF.md`. No runtime PR or staging deployment
has been made for this implementation branch.

## Implemented locally

- Financial eligibility accepts settled available credit **>= 0**, including
  zero, without suppressing any independent debt, invoice, usage, payment,
  refund, dispute or missing-evidence blocker. Deletion requires exactly zero.
- Forward migration `049_ownership_handoff_preparation.sql` adds evidence-backed
  responsibility periods and durable operation records. It does not backfill
  historical owners or alter previously released migration files. Account Manager
  remains the authority; this is a projection, not another owner-setting API.
- Trusted store bootstrap requires an explicit effective boundary and evidence
  digest, is replay-safe and refuses to overwrite existing owner/evidence.
  One open responsibility period per account is enforced by the database.
- Prepare binds operation/cloud/source/target/ownership version/cutoff, derives a
  request digest and serializes on the commercial account. Same-payload retries
  return durable state; changed payloads and competing operations conflict.
  Missing or mismatched responsibility evidence fails closed. Operation lookup
  requires both cloud and operation identifiers. Preparation and its audit are
  atomic. **Preparing is not a settled snapshot or authority to commit ownership.**
- Active preparation fences manual/hosted top-ups (including old-key retries),
  direct credit insertion, consent/method creation, policy changes, new automatic
  intents, new worker charge dispatch and hosted method setup. Existing provider
  work remains reconcilable. A recovered incomplete charge is queried, not charged
  again. Internal ledger debits remain available for cutoff settlement; producer
  holds and completeness checkpoints are still required before any readiness.
- Pending/hosted/unknown method setups are atomically invalidated with their
  pending method and consent. Late callbacks persist deduplicated hashed evidence
  without card/provider credentials and cannot reactivate the method, even after
  eventual cancellation. Authenticated simulator callbacks are acknowledged after
  persistence; browser setup failures remain explicit HTTP 409 conflicts.
- No timer, balance change, or commercial access-state change releases a fence.
- Forward migration `050_handoff_settlement_snapshots.sql` adds append-only
  settlement receipts, nonnegative balance snapshots and participant confirmations.
  Collector receipts bind operation/cloud/ownership version and a local financial
  state digest to the three independently reconciled domain checkpoints. A true
  completeness flag without its checkpoint is rejected. No browser input route
  can supply these receipts.
- Local invoice/debt/payment/setup/refund work can only add blockers to collector
  evidence, never be masked by supplied zero counts. A positive-total invoice's
  settled label is insufficient without its posted ledger linkage. Failed
  reconciliation jobs remain blockers until resolved, not equivalent to completed.
- Freshness checks include balance/version, usage, periods, invoices/settlement
  links, payment intents/jobs/attempts, setup observations and mapped webhooks.
  Every preview/confirmation recomputes local state. A changed amount (including
  +1 to 0 or 0 to -1), new usage or new evidence revision invalidates old approvals.
  Negative cutoff credit produces no confirmable snapshot. Old receipts/snapshots/
  approvals remain immutable evidence, not reusable authorization.
- Source/target confirmations independently validate identity, cloud, ownership
  version, phase, snapshot version and exact currency/amount. Concurrent retries
  persist once per participant/version with atomic audit. Both confirmations leave
  ownership and the monetary fence unchanged; they do not authorize owner commit.
- Forward migration `051_handoff_commit_protocol.sql` adds immutable commit
  authorizations, observed Account Manager committed decisions, finalization
  acknowledgments, precommit cancellation receipts and hold-release acknowledgments.
  Authorization rechecks the live snapshot and both stored confirmations. SQL
  account-serialized barriers prevent post-authorization ledger/balance, usage,
  invoice, period and settlement-link mutations from changing the confirmed amount.
  Provider inbox evidence is still writable; producer outboxes must retain rejected
  writes for reconciliation rather than treating the fence as permission to drop them.
- Finalization first persists the trusted AM committed decision in a separate
  transaction, entering `finalizing`. A later audit/profile/consent failure cannot
  erase that decision or make cancellation legal again. Local finalization then
  closes/opens responsibility periods, records the unchanged opening balance,
  revokes old consents/methods and disables automatic charging atomically with audit.
  Exact retries return the durable acknowledgment without repeating side effects.
  Logical grant/version binding establishes ordering; independent host wall clocks
  are not compared as a substitute for AM's committed decision.
- Prospective source profiles are archived in restricted immutable evidence.
  The active profile is reset to blank, `requires_configuration=true`, and the new
  ownership version, with a newer ETag. Invoice issuance is blocked until configured;
  draft arithmetic/usage accounting may continue without copying predecessor PII.
  Historical invoice recipient snapshots and ledger balances are not rewritten.
- Abort requires a trusted AM precommit cancellation receipt bound to any issued
  commit grant, followed by explicit producer-hold release acknowledgment. The
  interval remains fenced. Abort never restores revoked methods/consents or a
  policy disabled while held. Once a committed decision is observed, only forward
  finalization/reconciliation is permitted, never rollback to source ownership.

## Verification

Preparation tests used the isolated loopback PostgreSQL `multicloud_billing_test`
database on port 63229. Snapshot testing uses the separate fresh database
`multicloud_billing_settlement_test` on that same local container, including all
migrations from scratch. Neither is staging or shared development data. Use the
new commit test database `multicloud_billing_commit_test` for subsequent work.
It applied the final 049/050/051 migrations from scratch. The earlier local test
databases applied intermediate unpublished migrations and are not the current
final-migration evidence source.

- Full `go test ./...` passed after the initial preparation/fence implementation.
- API/store handoff tests repeated three times passed, including authenticated
  late-callback acknowledgment and deduplication.
- Coverage includes missing/stale evidence, changed-payload replay, same-operation
  concurrency, competing operations, one current responsibility period, unchanged
  balance during prepare, audit-failure rollback, invalidated setup revocation,
  new-charge/top-up denial and preserved payment reconciliation.
- `go vet ./...` and `git diff --check` passed at the initial checkpoint.
- Final full suite passed with the signed-callback and worker-recovery tests:
  `internal/paymentstore` 7.974s, `internal/paymentservice` 5.570s.
- Targeted `go test -race` passed for API (2.655s), payment store (2.879s) and
  payment worker service (2.110s). These are correctness checks, not benchmarks.
- Final `go vet ./...` and `git diff --check` passed.
- A final uncached `go test ./... -count=1` also passed (payment store 10.055s,
  worker service 8.609s); log `/tmp/rtk-billing-handoff-suite-uncached-20260831.log`.
- Local logs: `/tmp/rtk-billing-handoff-suite-final-20260831.log`,
  `/tmp/rtk-billing-handoff-repeated-20260831.log`,
  `/tmp/rtk-billing-handoff-race-20260831.log`.

Snapshot checkpoint evidence:

- Three repeated database runs passed for all handoff/initial-responsibility tests.
- Full uncached `go test ./... -count=1` passed on the fresh snapshot database;
  payment store 14.629s and worker service 8.537s (concurrent test runs share the
  fixture lock, so these elapsed times are not benchmarks).
- Final targeted race runs passed: payment store 10.984s, worker service 4.189s
  and API 4.902s. These include failed refund reconciliation remaining blocked.
  Logs: `/tmp/rtk-billing-settlement-repeated-20260831.log`,
  `/tmp/rtk-billing-settlement-suite-final-20260831.log`,
  `/tmp/rtk-billing-settlement-race-final-20260831.log`.
- Final `go vet ./...` and `git diff --check` passed.
- The receipt tests deliberately supply **synthetic trusted-collector evidence**.
  They prove protocol persistence, fail-closed local checks, replay/freshness,
  audit atomicity and nonnegative amount rules, not real producer/provider
  settlement completeness. Production collector integration is still outstanding.

Commit protocol checkpoint evidence:

- Fresh-database full `go test ./... -count=1` passed, including profile reset and
  returning-owner periods (payment store 12.793s, worker service 8.822s).
- API/store handoff tests repeated three times passed. Targeted race checks passed
  for payment store, worker service and API. Full/race durations are not benchmarks.
- `go vet ./...`, `git diff --check` and the service's OpenAPI import regression
  test (`python3 -m unittest discover -s scripts -p 'test_extract_openapi.py'`) passed.
- Logs: `/tmp/rtk-billing-commit-suite-final-20260831.log`,
  `/tmp/rtk-billing-commit-repeated-20260831.log`,
  `/tmp/rtk-billing-commit-race-20260831.log`.
- Tests prove local atomicity, immutable replay, current source-period checks,
  account-serialized commit barriers, durable commit observation before failed
  finalization, no post-observation cancellation, unchanged opening balance,
  revocation/profile reset and new periods for returning owners. They do not prove
  production AM decision authentication, membership mutation, historical-read
  privacy or predecessor compensation handling; those remain deployment gates.

## Not implemented / deployment gate

There is deliberately no externally callable prepare/bootstrap/collector route yet
and no production cross-service coordinator. Store-level commit/finalize/abort
logic is implemented, but trusted collectors/AM decision delivery are not connected.
Do not enable transfers or bootstrap historical responsibility from a today's-owner
lookup. Synthetic AM receipt hashes in protocol tests are not real owner mutations.

Remaining work includes:

1. Dedicated internal handoff credential, Account Manager durable coordinator,
   outbox, producer hold/cutoff acknowledgments, persisted usage/invoice/provider
   reconciliation checkpoint collectors and routing of versioned snapshots and
   both confirmations. The collector must observe local state before gathering
   independently verified checkpoints, not append a fresh digest to stale reports.
2. Connect commit/finalize/abort receipts to authenticated AM durable decisions and
   retry workers. Add the audited forward-reconciliation clearance path for changed
   provider evidence after an observed commit; currently such drift keeps the
   operation `finalizing` and cannot be canceled, rather than silently releasing it.
3. Current-owner/ownership-version enforcement and responsibility-period privacy
   for all Billing reads, aggregates, downloads, payment metadata and hosted returns.
   Existing tenant permission-header authorization is not sufficient.
4. Predecessor-period adjustment ledger, unknown-liability exceptions and delayed
   refund/chargeback handling that cannot debit successor spendable balance.
5. Resource-producer and new-work fences, closure protocol, full cross-service
   acceptance, migration preflight/restore, CI and staging evidence.

Preparing records and simulated terminal phase changes in low-level tests do not
prove an end-to-end ownership transfer. Existing cutoff debit ingestion is not
ingestion-completeness proof. Outstanding work must keep readiness fail-closed.
Snapshot freshness currently hashes per-cloud financial rows; scale evidence,
production producer holds, owner/version access enforcement, historical privacy,
and predecessor compensation routing must be established before enabling the
protocol endpoints. The SQL commit barrier does not prove producer completeness.
A bare persisted `prepared` phase is never a substitute for live financial status
or the explicit commit authorization protocol.
