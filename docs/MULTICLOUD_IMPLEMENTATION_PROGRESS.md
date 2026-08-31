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

## Verification

Preparation tests used the isolated loopback PostgreSQL `multicloud_billing_test`
database on port 63229. Snapshot testing uses the separate fresh database
`multicloud_billing_settlement_test` on that same local container, including all
migrations from scratch. Neither is staging or shared development data. Use the
new snapshot test database for subsequent work: the older local database applied
an intermediate, unpublished 050 and is not the final-migration evidence source.

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

## Not implemented / deployment gate

There is deliberately no externally callable prepare/bootstrap/collector route yet
and no owner-commit/finalize/abort implementation. Trusted collector storage can
advance a preparation to a confirmable snapshot, but no production collector is
connected. Do not enable transfers or bootstrap historical responsibility from a
today's-owner lookup.

Remaining work includes:

1. Dedicated internal handoff credential, Account Manager durable coordinator,
   outbox, producer hold/cutoff acknowledgments, persisted usage/invoice/provider
   reconciliation checkpoint collectors and routing of versioned snapshots and
   both confirmations. The collector must observe local state before gathering
   independently verified checkpoints, not append a fresh digest to stale reports.
2. Commit authorization, permanent old-payment-consent revocation, profile retirement
   and reset, responsibility close/open, durable finalize/abort acknowledgments.
   Owner-commit failures must finish forward, not restore predecessor access.
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
Snapshot freshness currently hashes per-cloud financial rows; scale evidence and
the final synchronized producer/write fence must be established before enabling
commit. A bare persisted `prepared` phase is never a substitute for live financial
status or the later commit authorization protocol.
