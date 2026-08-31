# Multi-cloud Billing implementation checkpoint

This is implementation evidence, not replacement normative design. The approved
target remains `cloud_ownership_handoff.md`. No runtime PR or staging deployment
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
- Forward migration `052_provider_reversal_responsibility.sql` binds new payment
  intents to the proven responsibility period at creation. It serializes on the
  account and rejects new intents during handoff even through direct SQL. Existing
  intents are deliberately **not** backfilled from current ownership. Reviewed
  migration bindings require explicit evidence, a reviewer and atomic audit;
  bindings cannot subsequently be reassigned.
- Verified-worker reversal storage deduplicates provider/event identity across
  accounts, binds the original settled payment and its responsibility period,
  checks cumulative refunded/charged-back amounts under the account lock and
  atomically records attribution and audit. Generic `PostLedgerEntry` no longer
  accepts refund/chargeback reasons; it cannot bypass attribution.
- Current-period reversals debit once and disarm auto-top-up without starting a
  replacement charge. Predecessor reversals use a separate append-only adjustment
  ledger, never the successor's spendable balance or payment policy. This uses
  period IDs rather than owner IDs, including returning owners and closed clouds.
  Unknown, cross-cloud, unpaid, unproven or excessive reversal claims are retained
  as audited reviews without touching money. Provider adapters must supply trusted
  verified receipts; a supplied digest alone does not verify a provider signature.
- Unresolved reversal reviews are local financial blockers and part of snapshot
  freshness; collector zero counts cannot mask them. During commit authorization,
  new reversals retain an unresolved review rather than changing confirmed money.
  Once the durable AM commit decision is observed, audited resolution can classify
  the original-period adjustment and allow exact finalization retry with unchanged
  opening balance. Unresolved reviews and unrelated financial drift remain fenced.
  This is a narrow forward-recovery path, not general permission to ignore drift.
- The HTTP server now requires an ownership verifier for all 22 tenant operations,
  in addition to service authentication, global actor context, exact permission
  and Billing access-state checks. `X-Billing-Ownership-Version` is mandatory and
  must match the current evidence-backed sole-owner projection. Claimed role or
  platform headers do not upgrade tenant authority. Missing account/responsibility
  evidence fails closed without lazy account provisioning; commit-in-progress
  blocks tenant access until finalization or acknowledged cancellation completes.
- Verified organization/account/user/version context reaches payment transactions.
  Account-locked mutations recheck it against the same row lock as handoff commit.
  Account reads and Billing profile get/ensure/update also revalidate inside their
  transaction. A request admitted before transfer cannot write afterward; a returning
  owner's old version does not revive. Profile reads/updates additionally require
  proven matching profile ownership version; unproven legacy profiles are neither
  disclosed, overwritten nor automatically adopted from today's owner.
- Service OpenAPI and its import regression guard document the mandatory version
  header on every tenant path. Production BFF propagation/bootstrap integration
  has **not** been implemented yet, so this branch must not be deployed alone.
  The entry checks are complemented by the period-scoped reads below; entry
  authorization alone does not authorize a predecessor's financial history.
- A validated provider setup response is reconciled independently of the now-stale
  tenant request, preserving invalidation evidence without reactivating old methods.
  Hosted setup/checkout responses recheck owner/version before exposing actions to
  the browser. A synchronous setup response arriving after local finalization is
  tested to record evidence while returning an invalidated-setup conflict without
  the old hosted URL. This does not yet implement the BFF hosted-return binding.
- Forward migration `053_financial_record_responsibility.sql` binds newly created
  payment methods and ledger entries to proven responsibility periods. Payment
  credits inherit the original intent binding, invoice debits require a matching
  recipient version and whole-period containment, and current manual
  adjustments use the current period. Unproven legacy records are not backfilled.
  These bindings are append-only; they do not change monetary liability or permit
  late predecessor usage to debit a successor. Production cutoff routing is still
  a separate deployment gate.
- Invoice detail, PDF bytes, lists/counts, invoice CSV statements, activity and
  timelines, ledger and payment-method/intent/attempt reads now enforce proven
  responsibility-period visibility. Whole mixed-period or unproven invoices are
  withheld. Returning owners can read their own earlier periods but not intervening
  periods; departed owners cannot read even their own history through tenant APIs.
  Related records and each page/count share a repeatable-read, account-locked
  authorized snapshot. Authority/fence transitions touch the guard row so a reader
  with an older PostgreSQL snapshot cannot retain old authority after waiting for
  that lock. Such conflicts return retryable `BILLING_SNAPSHOT_CONFLICT`, not data.
- Usage is period-filtered before pricing, counting or aggregation. A window must
  fit wholly inside one proven caller period; unknown/cross-period windows are
  withheld, never guessed or prorated. Summary/forecast inputs start at the later
  of month start and current tenure start. Explicit historical queries can include
  earlier own periods, but not predecessor/intervening quantities or costs.
- The current auto-topup projection excludes prior-period methods. DELETE and
  method mutations enforce the same current-version boundary, even for revoked
  rows. A new owner can configure a proven retired policy with their own new
  method and consent, using logical-create version zero. The old projection is
  archived in append-only restricted evidence, creation metadata resets and
  policy generations/versions remain monotonic. Current configuration needs an
  exact version; unknown legacy configuration cannot be silently adopted.
  Manual payment completion no longer rearms a disabled predecessor policy.
- Manual and hosted top-up retries additionally require the original intent's
  binding to the current ownership version. Possessing an old idempotency key
  cannot reveal predecessor or previous-tenure transactions; current retries
  remain idempotent. Provider reconciliation continues through its original
  separately trusted binding, not tenant history authorization.

## Verification

Preparation tests used the isolated loopback PostgreSQL `multicloud_billing_test`
database on port 63229. Snapshot testing uses the separate fresh database
`multicloud_billing_settlement_test` on that same local container, including all
migrations from scratch. Neither is staging or shared development data. Use the
commit test database `multicloud_billing_commit_test` established the 051 boundary.
Current reversal testing uses the fresh `multicloud_billing_reversal_test` database
on the same loopback container, applying all migrations through 052 from scratch.
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

Provider reversal checkpoint evidence:

- Full uncached `go test ./... -count=1` passed on the fresh reversal database
  (payment store 26.883s, worker service 11.615s). All reversal/handoff/financial
  predicate tests passed three repeated runs (store 43.077s); targeted race checks
  passed (store 14.221s, Billing predicates 1.687s). Concurrent suites serialize
  integration fixtures with the database advisory lock; durations are not benchmarks.
- `go vet ./...` and `git diff --check` passed. Logs:
  `/tmp/rtk-billing-reversal-suite-final-20260831.log`,
  `/tmp/rtk-billing-reversal-repeated-20260831.log`,
  `/tmp/rtk-billing-reversal-race-final-20260831.log`.
- Coverage includes duplicate/concurrent receipts, competing partial refund and
  chargeback caps, changed-event replay rejection, cross-cloud and unpaid claims,
  missing historical binding, reviewed binding/resolution replay, append-only
  evidence, unchanged successor/returning-owner/closed-account balances and policy,
  commit-observed forward resolution, unmaskable unknown events, new-charge SQL
  fencing, amount reconfirmation and audit-failure rollback with retry.
- These tests use synthetic paid-provider, collector and AM evidence. No provider
  signature adapter, actual AM owner mutation, tenant financial-privacy enforcement
  or staging acceptance is implied. No shared/staging database was changed.

Owner-boundary checkpoint evidence:

- Full uncached `go test ./... -count=1` passed (API 4.408s, payment store 26.430s,
  worker service 12.725s). Targeted race checks passed (API 8.345s, store 21.419s).
  Owner-boundary and in-flight provider tests passed three repeated runs (API
  4.280s, store 3.590s). Final tenant `Cache-Control: no-store` response handling
  was additionally checked by the focused database-backed API tests.
- `go vet ./...`, `git diff --check`, and the OpenAPI import regression passed.
  Logs: `/tmp/rtk-billing-owner-suite-final-20260831.log`,
  `/tmp/rtk-billing-owner-race-final-20260831.log`,
  `/tmp/rtk-billing-owner-repeated-final-20260831.log`.
- The runtime route inventory drives negative tests across all 22 tenant
  operations for admin/member/viewer/other-cloud-owner/transfer-target/platform-only
  actors, even with all Billing permission and claimed-owner headers. Additional
  tests cover malformed/stale versions, missing projections without provisioning,
  request contexts surviving transfer/return, cross-cloud account/profile attempts,
  unknown legacy profile preservation and late verified setup evidence without
  browser access. Fixtures provision synthetic ownership explicitly, never as a
  request-helper fallback; production identity/bootstrap integration remains absent.

Financial privacy checkpoint evidence:

- A fresh isolated `multicloud_billing_privacy_guard_test` database on loopback
  port 63229 applied every migration through the final unpublished 053. Earlier
  `multicloud_billing_privacy_test` / `multicloud_billing_privacy_final_test` databases
  contain intermediate 053 versions and are not final-migration evidence.
- Full uncached `go test ./... -count=1` passed (API 9.997s, payment store 20.140s,
  worker service 12.802s). Final targeted race checks passed (API 4.674s, store
  11.066s). These are correctness checks, not scale benchmarks. Privacy/guard
  tests also passed three repeated runs; the additional unknown-policy/hosted-key
  test passed separately and in the final full/race suites.
- `go vet ./...`, `git diff --check` and the OpenAPI import regression passed.
  Final logs: `/tmp/rtk-billing-privacy-suite-final-20260831.log`,
  `/tmp/rtk-billing-privacy-race-final-20260831.log`, and repeated-run log
  `/tmp/rtk-billing-privacy-repeated-20260831.log`.
- Database-backed HTTP coverage includes two transfers A→B→A, direct IDs/PDF
  bytes/CSV, lists/counts/pagination, hidden predecessor and unproven records,
  mixed usage/invoice windows, current-period summary/forecast and old method
  mutation denial. It also covers retired-policy replacement with new consent,
  immutable policy evidence, blind-overwrite rejection and old-key replay denial.
  A deterministic pre-fence PostgreSQL snapshot test verifies that waiting with
  an old snapshot cannot retain authority; fresh fenced/departed reads fail too.
- Fixtures use synthetic trusted collector/AM receipts and deliberately identifying
  fake PDF bytes. They prove local authorization and privacy, not production
  collector completeness, PDF rendering, real AM membership changes, migration
  reconciliation, BFF/browser scope isolation or staging acceptance. No shared or
  staging database was changed; this branch still must not be deployed alone.

## Dedicated HTTP transport checkpoint — 2026-08-31

- `BILLING_HANDOFF_TOKEN` optionally registers six coordinator endpoints: prepare,
  live settlement, participant confirm, authorize-commit, finalize and begin-abort.
  Startup rejects weak/reused credentials across tenant, internal, debit,
  provider and reference-encryption boundaries. Unconfigured routes remain 404;
  no staging secret or deployment was changed.
- Every route authenticates the dedicated bearer and exact original ownership
  version, binds cloud/operation, rejects unknown/trailing/oversized JSON and uses
  a bounded context. Responses echo the scope, are non-cacheable and suppress
  backend diagnostics. General tenant/internal/debit credentials cannot enter;
  the handoff credential cannot enter tenant or pricing/access APIs.
- The coordinator API does not expose initial responsibility, settlement-receipt
  ingestion or hold-release certification. Billing's persisted evidence remains
  authoritative: request flags cannot mint readiness or both confirmations.
  Abort requests remain `abort_pending` until trusted hold-release evidence arrives.
- HTTP tests cover -1/0/+1, missing evidence, exact/changed amount, source-only
  confirmation, outsider, stale snapshot/version/cloud, duplicate requests and
  finalization. Injected finalize-audit failure returns 503 but leaves the durable
  known AM decision `finalizing`; abort still conflicts and exact retry completes.
- Service OpenAPI describes typed requests/results and the dedicated security
  scheme. The compatibility importer now preserves that fourth boundary instead
  of recategorizing these routes as general internal Billing. Import tests pass.
- A loopback TCP test invokes the separately compiled AM client in
  `/tmp/rtk-multicloud-impl.bJ3Ic4`; prepare/status/confirm/grant/finalize/retry/abort
  travel through the actual HTTP router into PostgreSQL. Three repeated race runs
  of HTTP/client-contract cases passed (API 9.517s). Initial broader boundary/config
  race tests passed three repeats (API 18.510s, config 1.666s).
- All fixtures use isolated `multicloud_billing_transport_test` on port 63229,
  including migrations through the existing 053. No new Billing migration is needed
  for the transport. **Collector and AM decision receipts are synthetic**; there
  is no real AM membership commit or resource/provider reconciliation in these tests.
- Final full uncached suite passed with the cross-repository client fixture enabled
  (API 18.668s, payment store 33.054s, payment service 18.231s). Log:
  `/tmp/rtk-billing-transport-suite-final-20260831-r2.log`. `go vet ./...`,
  `git diff --check`, API security-scheme tests and the Python contract importer
  tests passed. These are local checks, not runtime PR CI or staging evidence.

## AM public API confirmation checkpoint — 2026-08-31

- Added optional `TestHandoffAccountManagerPublicAPIContract`: the separately
  compiled AM API creates verified global source/target sessions, requests and
  accepts a handoff via its real HTTP handlers, then previews and confirms the
  Billing snapshot through this service's real dedicated HTTP router/store.
- The two participants confirm zero credit, retry independently and reload the
  durable result. Billing stores exactly two confirmations and remains prepared;
  AM stores two intents/acknowledgments, retains source ownership and denies the
  target ordinary membership before commit. No amount moves and no owner switches.
- Set `ACCOUNT_MANAGER_HANDOFF_CLIENT_DIR` to the isolated AM checkout and
  `ACCOUNT_MANAGER_HANDOFF_DATABASE_URL` to the dedicated disposable
  `multicloud_am_public_http_test` database at `127.0.0.1:63229`. Billing continues
  using its own isolated `TEST_DATABASE_URL`; the databases must be separate.
  The test-only bind endpoint belongs to an `httptest` wrapper, not production.
- Initial eligibility, producer preparation and collector completeness are
  synthetic. The test proves AM session-to-Billing confirmation persistence,
  not actual producer drain, real settlement completeness, owner commit, browser
  behavior or staging acceptance. No runtime configuration was changed.
- Full uncached `go test ./... -count=1` passed with both cross-repository fixtures
  enabled (API 33.334s, payment store 44.505s, payment service 32.013s). The new
  public-API fixture passed three Billing-side race runs (13.555s); AM's own full
  suite also passed. Logs: `/tmp/rtk-billing-public-handoff-suite-20260831.log`
  and `/tmp/rtk-billing-public-handoff-race-20260831.log`. `go vet ./...`,
  `git diff --check` and the OpenAPI importer regression passed. These are local
  correctness checks, not runtime CI or performance measurements.

## Actual AM owner decision integration checkpoint — 2026-08-31

- The public-API cross-repository fixture now runs both consent-only and complete
  commit/finalize variants. In the latter, AM persists a durable grant request,
  consumes this Billing service's actual authorization, atomically changes the
  unique owner/version and records its committed decision. That real decision
  then reaches Billing over the dedicated HTTP client and is finalized/replayed.
- The fixture verifies AM global-session status and old/new owner access, two
  Billing responsibility periods and exactly one current target/version. Both
  sides persist real protocol state rather than passing a synthetic AM commit hash.
  Initial financial eligibility, collector completeness and producer preparation/
  release still use synthetic evidence. This does not prove production settlement,
  producer drain, automatic worker delivery, UI or staging acceptance.
- Full uncached Billing `go test ./... -count=1` passed with both variants enabled
  (API 31.226s, payment store 40.166s, payment service 29.533s). Log:
  `/tmp/rtk-billing-real-owner-commit-suite-20260831.log`. No new Billing migration
  or runtime configuration was needed; AM's isolated databases applied forward 058.
- Both fixture variants passed three Billing-side race runs (23.104s). Log:
  `/tmp/rtk-billing-real-owner-commit-race-20260831.log`. AM's owner-commit store
  tests passed separate repeated race runs; both services passed `go vet ./...`
  and `git diff --check`. No runtime PR, deployment or shared data change occurred.

## Automatic AM worker and cancellation race checkpoint — 2026-08-31

- The cross-repository public API fixture now also runs AM's actual database-backed
  recovery worker. Persisted jobs advance confirmed handoffs through real AM owner
  commit and Billing HTTP finalization without manually invoking those store steps.
  Prepared/released resource evidence and collector completeness remain synthetic.
- Precommit abort may include the durable attempted AM authorization ID before
  Billing has issued a grant. If a grant exists it must still match exactly. The
  cancellation digest retains that same attempted ID across retries, abort-pending
  blocks a late authorize request, and a changed cancellation payload conflicts.
  This avoids guessing grant issuance after an ambiguous authorization response.
- Focused cancellation tests pass, including valid source/target confirmations
  followed by cancellation before grant issuance and denial of the late grant.
  Abort still requires separate trusted release certification; the HTTP response
  `abort_pending` is not authority to release AM or producer fences.
- Full uncached `go test ./... -count=1` passed with consent/manual-commit/worker
  cross-repository variants enabled (API 33.059s, payment store 41.917s, payment
  service 30.589s). Log: `/tmp/rtk-billing-handoff-worker-suite-20260831.log`.
  No Billing schema, shared database or production configuration was changed.
- The three-mode API fixture and abort cases passed three race runs (API 26.851s,
  payment store 19.607s), recorded in `/tmp/rtk-billing-handoff-worker-race-20260831.log`.
  `go vet ./...`, `git diff --check` and the OpenAPI importer regression passed.
  AM separately passed full regression and repeated store/worker/config race tests.

## Advisory cloud deletion preflight (054)

- Added forward migration 054 for immutable current-owner preflight collector
  receipts, separate from frozen handoff settlement/commit evidence. Receipts
  bind account, owner/version, the captured local financial-state digest and
  independently reconciled usage/invoice/provider checkpoints with a maximum
  five-minute observation lifetime. Changed local state invalidates a receipt;
  external zeros cannot remove independently observed local financial blockers.
- Read-only preflight uses a repeatable snapshot and checks current responsibility.
  It never ensures/creates an account, installs a hold, writes a receipt or closes
  Billing. Exact zero credit is required alongside all other financial clearance.
  Active handoffs and nonactive account state block deletion. Missing/stale
  collector evidence is never treated as readiness from empty database tables.
- Added dedicated-coordinator `GET /v1/internal/billing/clouds/{orgId}/deletion-preflight`.
  It requires the existing isolated handoff credential plus current owner and
  ownership-version headers. Tenant/debit credentials cannot call it. No HTTP
  collector-write or closure route was exposed. OpenAPI and its compatibility
  importer preserve this credential boundary.
- Store/HTTP tests cover -1/0/+1 credit, missing checkpoints, stale local state,
  expired evidence, owner/version mismatch, immutable replay, independent
  debt/payment/refund/dispute blockers and no side effects from preflight.
  A cross-repository fixture runs the actual AM preflight HTTP client against
  Billing's router/store. Reconciliation checkpoints remain explicit synthetic
  fixtures; no real collector or producer completeness is claimed.
- Full uncached Go suite passed against fresh isolated
  `multicloud_billing_preflight_test` through 054 (API 47.714s, paymentstore 57.717s,
  paymentservice 43.022s). Targeted API/paymentstore race tests passed three runs
  (12.077s / 18.012s), including the real AM transport fixture. Logs:
  `/tmp/rtk-billing-deletion-preflight-suite-20260831.log` and
  `/tmp/rtk-billing-deletion-preflight-race-20260831.log`. Vet, whitespace checks
  and the OpenAPI compatibility-importer unit test also passed. The existing
  consent/commit/worker cross-repository fixtures ran in the full suite.
- This does not implement Billing closure or authorize AM soft deletion. The
  later lifecycle fence, settled cutoff, provider cancellation and durable closure
  acknowledgments remain required before any actual delete. No runtime PR,
  shared-database mutation or staging deployment was performed.

## Durable Billing cloud closure (055)

- Forward migration 055 adds account-serialized closure operations, immutable
  provider-revocation manifests/receipts, cutoff settlement evidence, completion
  receipts and cancellation/release acknowledgments. Active closure and ownership
  handoff are mutually exclusive. Prepare requires AM's persisted deletion intent
  and resource fence; it is not deletion permission or settlement certification.
- Preparation atomically invalidates pending setups, revokes local methods and
  consents, disables/disarms auto charging and writes audit evidence. New payer
  authority and charge dispatch are blocked through preparing/canceling/closed.
  Cutoff reconciliation remains possible before close. Late setup callbacks retain
  deduplicated hashed observations but cannot restore credentials or authority.
- Local revocation is not provider cancellation. Each method/setup manifest task
  needs independently verified provider acknowledgment. Settlement receipts bind
  the operation, original owner/version, cutoff, local financial state and current
  provider acknowledgment manifest, with independently reconciled checkpoints and
  a maximum five-minute lifetime. Missing, expired, changed or misbound evidence
  fails closed; supplied zeros cannot suppress local financial blockers.
- Close requires exactly zero balance and all other financial clearance. Under
  one account lock it rechecks live evidence, closes the Billing account/access,
  ends its responsibility period, and records immutable completion plus audit.
  SQL guards prohibit reopening or appending a new responsibility period and
  changing closed monetary history. Exact retries return the original receipt
  even after the responsibility period ends; changed replays conflict. AM must
  finish tombstoning by forward retry, never reopen Billing after this commit.
- Pre-close cancellation remains fenced until verified provider revocations and
  hold-release acknowledgment. Cancellation does not revive old payment methods,
  consent or auto charging. Revocation provenance cannot be cleared to restore a
  consent. A later setup requires new explicit payer authorization.
- Added dedicated-coordinator POST prepare/close/cancel and GET status under
  `/v1/internal/billing/clouds/{orgId}/closures/{operationId}`. Every command
  requires canonical cloud/operation IDs and exact owner/version headers, echoes
  that scope and uses no-store responses. Existing dedicated credential, bounded
  strict JSON and request deadline apply. OpenAPI and its compatibility importer
  preserve this boundary. There is no HTTP settlement-write, provider-ack or
  cancellation-release endpoint; unconfigured routes remain 404.
- Tests cover negative/zero/positive balances, stale cutoff debit/provider
  manifest, missing checkpoints, expired/misbound receipts, provider revocation,
  late callbacks, audit rollback, duplicate concurrent prepare/close, competing
  payer authority, terminal-state preservation, cancellation and HTTP credential
  isolation. Provider/collector and AM decision evidence are synthetic fixtures;
  these tests are not live-provider or complete cross-service deletion acceptance.
- AM durable DELETE/resource producer fences/closure transport, production
  reconciliation and provider adapters, UI operation tracking and staging evidence
  remain required. No runtime PR, shared-database change or deployment occurred.
- Verification: full uncached Go suite passed on fresh isolated
  `multicloud_billing_closure_v3_test` through migration 055 (API 25.788s,
  paymentstore 46.891s, paymentservice 23.038s), including existing real AM
  transport/public-API/worker fixtures. Targeted closure, handoff and deletion
  preflight race tests passed three runs. The final additional route-absence and
  concurrent-consent tests are included in that race run. Logs:
  `/tmp/rtk-billing-cloud-closure-v3-suite-20260831.log` and
  `/tmp/rtk-billing-cloud-closure-v3-race-20260831.log`. OpenAPI compatibility
  importer tests, `go vet ./...` and whitespace checks also passed.

## Permanent close-command retirement checkpoint (056)

- The dedicated coordinator API now includes `POST .../closures/{operationId}/retire-command`.
  It accepts the exact close settlement/readiness command and, under the same
  account lock as close, returns one durable outcome: permanently retired or the
  original matching closed acknowledgment. An HTTP rejection alone does not
  authorize AM to replace a command. Retirement never releases holds or creates
  settlement/provider evidence.
- Forward migration 056 adds immutable command-retirement receipts and SQL guards
  against completing a retired command. Delayed retries stay rejected even if the
  old settlement arrives later or a different command subsequently closes Billing.
  Replays recover the stored receipt; mismatched committed commands conflict.
  Receipt and audit commit atomically. Closed accounts are never reopened.
- OpenAPI specifies mutually exclusive outcomes and the importer retains the
  dedicated credential boundary. HTTP tests cover missing configuration, forbidden
  credentials and malformed bodies for the new route. No provider/collector or
  hold-release write endpoint is exposed by this change.
- AM now consumes retirement proof before replacing stale evidence or canceling.
  The four-mode cross-service fixture covers lost close response, stale evidence
  plus lost retirement response, partial cancellation followed by final release,
  and close winning cancellation. Both databases and HTTP/store implementations
  are real; collector/provider/resource receipts remain synthetic test fixtures.
  AM cancellation is currently a store command, not a public HTTP/BFF feature.
- Verification: the full uncached Billing suite passed through 056 on
  `multicloud_billing_closure_recovery_test` (paymentstore 113.950s,
  paymentservice 64.401s), including AM cross-service fixtures. Targeted closure
  race tests passed three runs (paymentstore 66.580s, API 24.107s), including eight
  close/retirement races per run and audit rollback. OpenAPI importer tests and
  `go vet ./...` passed. Logs: `/tmp/rtk-billing-deletion-recovery-suite-20260831.log`
  and `/tmp/rtk-billing-deletion-recovery-race-20260831.log`.
  All four cross-service modes passed three Billing race-instrumented runs
  (34.467s) using fresh `multicloud_am_deletion_recovery_http_test` through
  current AM 062; log: `/tmp/rtk-am-billing-deletion-recovery-cross-race-20260831.log`.
  The AM child is separately compiled and its own targeted race tests passed three
  runs, as recorded in AM's 062 checkpoint. These tests are not provider or staging
  acceptance.
  No runtime PR, shared-database change or deployment occurred.

## Not implemented / deployment gate

### AM deletion integration checkpoint

`TestCloudDeletionAccountManagerPublicAPIContract` now invokes the actual AM
global-session DELETE/status APIs in the isolated implementation checkout and a
separate disposable AM database. Real Billing preflight/prepare/status/close and
durable persistence are exercised. A test wrapper loses the first successful
close response; AM recovers the persisted job with a new store instance, retries
the original close command and completes its real tombstone. Billing remains
closed. The producer hold/inventory and collector checkpoints are explicitly
synthetic and installed only by the test wrapper; no production fixture route
was added. Three Billing race-instrumented runs passed; log:
`/tmp/rtk-am-billing-deletion-cross-race-20260831.log`. AM child tests compile and
run independently; their own race checks are tracked in the AM checkpoint.
The optional `ACCOUNT_MANAGER_DELETION_DATABASE_URL` selects the dedicated
`multicloud_am_deletion_http_test` database on loopback port 63229; the new recovery
fixture also permits fresh `multicloud_am_deletion_recovery_http_test` through AM
migration 062. This does not
remove production collector/provider/resource-adapter or staging release gates.

The optional dedicated handoff HTTP transport and separately compiled AM client
are now implemented and exercised together against isolated Billing persistence.
No handoff routes exist without explicit `BILLING_HANDOFF_TOKEN` configuration;
no production configuration was changed. There is still no production cross-service
coordinator and no coordinator bootstrap/collector/hold-release certification route.
Trusted collectors and production resource transports are not connected. AM's
automatic durable worker is now exercised with the real Billing command transport,
but it must not be enabled as a substitute for those remaining dependencies.
Do not enable transfers or bootstrap historical responsibility from a today's-owner
lookup. Synthetic AM receipt hashes in protocol tests are not real owner mutations.

Remaining work includes:

1. Wire the implemented dedicated transport to the Account Manager durable coordinator,
   outbox, producer hold/cutoff acknowledgments, persisted usage/invoice/provider
   reconciliation checkpoint collectors. AM public versioned preview and both
   confirmations are now wired; automatic coordinator delivery remains absent.
   The collector must observe local state before gathering
   independently verified checkpoints, not append a fresh digest to stale reports.
2. Deploy the tested AM retry worker only with authenticated production producer
   and abort/release delivery, after complete cross-service acceptance. Add the audited
   forward-reconciliation clearance path for changed
   provider evidence after an observed commit; currently such drift keeps the
   operation `finalizing` and cannot be canceled, rather than silently releasing it.
3. Connect the BFF's authenticated global actor and ownership version plus trusted
   signup/migration responsibility/profile provisioning. Existing accounts without
   evidence intentionally fail closed. Tenant period filters are now implemented
   locally, but reviewed historical method/ledger binding and migration preflight,
   separately audited platform history reads, the safe opening-balance UI projection,
   and hosted-return browser binding still need integration. Activity enumeration
   no longer silently truncates source rows at 500, but still materializes visible
   history for normalization/filtering. SQL-bounded pagination and realistic scale
   evidence are required before enabling this for large production accounts.
4. Connect the local predecessor adjustment/review store to signature-verified
   provider adapters, durable inbox/reconciliation workers and restricted platform
   exception/recovery tooling. Routing must not mutate generic payment-state inbox
   hashes for an already-classified predecessor adjustment and thereby strand an
   otherwise unchanged commit snapshot. Provider cash recovery, signatures and
   real cross-service receipt delivery are not proven by the store fixtures.
5. Production resource-producer and new-work fences, AM integration of the local
   Billing closure protocol, full cross-service acceptance, migration
   preflight/restore, CI and staging evidence.

Preparing records and simulated terminal phase changes in low-level tests do not
prove an end-to-end ownership transfer. Existing cutoff debit ingestion is not
ingestion-completeness proof. Outstanding work must keep readiness fail-closed.
Snapshot freshness currently hashes per-cloud financial rows; scale evidence,
production producer holds, integrated owner/version propagation and privacy,
and predecessor compensation routing must be established before production
enablement. The SQL commit barrier does not prove producer completeness.
A bare persisted `prepared` phase is never a substitute for live financial status
or the explicit commit authorization protocol.

## Read-only ownership eligibility transport — 2026-09-01

Added the dedicated internal ownership-eligibility route and independently
compiled AM client contract tests. The read-only repeatable snapshot checks
current responsibility, local financial state and fresh collector evidence;
proof binds the target/action/transfer as well as the cloud and owner version.
No query creates an account, hold, receipt or responsibility period. Transfer
permits zero or positive credit only with all independent checks satisfied;
deletion continues to require exactly zero. Missing collector evidence remains
unavailable rather than treating empty tables as reconciliation proof.

Focused store/API race tests passed (8.267s/7.404s), including actual HTTP calls
from AM for -1/0/+1 at request and acceptance, stale receipt invalidation,
independent financial blockers, unchanged read-only state and deletion policy.
Full uncached `go test ./...` passed on the owned loopback database
`multicloud_billing_eligibility_20260901`; `go vet ./...`, `go build ./...` and
the OpenAPI extractor unit test passed. Full-suite log:
`/tmp/rtk-billing-eligibility-full.log`; ordinary coverage profile:
`/tmp/rtk-billing-eligibility-full-coverage.out`. This profile is not the workspace
governed coverage gate. Production collector/provider/producer wiring, service
CI, migration rehearsal and staging acceptance remain open.
