# Brand Cloud Billing ownership handoff design

Status: design-first target, not deployed acceptance. Follow canonical
[multicloud_ownership.md](https://github.com/hkt999rtk/rtk_cloud_contracts_doc/blob/codex/multicloud-owner-design/multicloud_ownership.md). Account Manager owns membership/ownership; Billing owns
monetary state using opaque organization UUIDs without cross-database joins.

## Responsibility and access

Cloud accounts/balances remain separate even under one owner. Add versioned
responsibility periods binding cloud, global owner ID, ownership version,
effective boundary and committed operation. These are Account Manager projections,
not a second ownership administration interface. Unknown historical responsibility
requires evidence/review, not attribution to today's owner. Invoice/legal identity
snapshots remain immutable; late adjustments retain their original period.

Tenant reads/writes require current sole-owner authority and exact permission.
BFF headers cannot bypass a fence or ownership-version mismatch. Platform access
is separately authorized/audited. Old broad admin/member Billing grants confer
no tenant access under this model; another cloud's owner has no implicit rights.
This intentionally supersedes legacy billing-viewer/admin/member reads: no
collaborator receives tenant Billing read or write access, even if an old ACL
assignment contains a Billing permission. Platform access is separately audited.

Before ownership commit, the source remains the sole designated owner; the
target is only a transfer participant, not a second designated owner. Account
Manager authenticates source/target sessions and grants participant-only access
to that transfer's status, settled amount/version preview and confirmation routes.
It checks operation ID, cloud, source/target identity, ownership version, phase
and expiry on every call. Target gets no general cloud, Product or Billing access.
Billing serves the minimal snapshot to Account Manager using the dedicated
handoff credential; browsers cannot call internal Billing handoff commands.
Confirmation only acknowledges that snapshot, not a payment or owner mutation.

Every UI/BFF billing action, read, idempotency key, confirmation and hosted-payment
return is bound to the initiating cloud UUID, actor and ownership version. Validate
that immutable binding against current authority; a shared active-cloud session
cannot override it. Reject stale callbacks/forms after transfer or scope change.
Provider callbacks reconcile their original intent/cloud binding without granting
the browser access. Cloud switches discard stale responses and isolate caches.

Current ownership alone does not expose predecessor financial records. Tenant
invoice/document/statement/activity/ledger reads, payment intents and attempts,
payment-method metadata, list totals, exports and download URLs
are additionally restricted to the caller's recorded responsibility periods.
This includes usage facts, quantities, costs, summary `current_period`, forecasts
and aggregates for any requested date range. Clip to eligible responsibility
intervals before aggregation; neither historical range queries nor a partially
overlapping billing month may expose predecessor usage/totals. Forecasts use
only authorized inputs or return unavailable, not an estimate derived from hidden
history. The confirmed opening balance is a separate safe projection, not access
to the ledger entries or usage facts that produced it.
An owner returning after an intervening owner cannot read the intervening period.
Unknown or mixed-period records require a safe period-specific projection or are
withheld; never disclose another payer's legal name, tax ID, address, email,
payment reference or line-item history. The incoming owner receives only the
confirmed opening balance, handoff boundary and subsequent own-period records,
not predecessor invoices or identifying ledger detail. Keep the full immutable
history for separately authorized/audited platform access. The departed owner
has no tenant access, including historical records; required document delivery
uses existing verified billing delivery/support, not restored cloud membership.

## Durable financial boundary

Transfer requires the current owner to settle all outstanding Billing and leave
nonnegative available cloud credit (`balance_minor >= 0`). Zero is eligible.
Return `balance_negative` for negative credit; independently
reject unrated/unsettled usage, unpaid invoices/debt, pending payments/refunds/
disputes and unavailable evidence even if the balance projection is positive.
Request/acceptance eligibility checks are advisory to the later fenced financial
commit, not permission to use stale snapshots. Recheck complete cutoff settlement
and the exact nonnegative balance/version before authorizing ownership commit.

Internal prepare/status/finalize/abort commands bind cloud ID, operation ID and
expected ownership version. Use a dedicated handoff service credential, distinct
from tenant, pricing/access, debit and provider credentials. Store request digest,
state, cutoff/checkpoint, source/target user, confirmed amount/currency/version,
consent disposition and acknowledgments. Identical retries are replay-safe;
changed payloads or stale versions conflict. The browser cannot supply trusted
ownership or call internal handoff routes.

Prepare installs a monetary fence excluding new charges/top-ups and auto-topup.
Also fence hosted payment-method setup creation/completion. Bind each setup session
and pending method to its original actor/cloud/ownership version; terminalize them
before preparation readiness. Late or duplicate provider setup callbacks can only
record unusable evidence, never activate a method or restore consent after handoff.
Unresolved setup/provider work blocks readiness. Reconciliation remains available.
Already dispatched provider work must reconcile; a local fence cannot cancel an
external charge. Resource producers acknowledge cutoff, ingestion completeness
and settlement. Unrated/unbounded late usage, debt, payments/refunds/disputes or
missing evidence blocks preparation. Time or zero invoice count is not proof.
Cutoff settlement leaving negative credit blocks preparation/commit and
produces no confirmable balance snapshot. Keep ownership unchanged and holds
active; finish precommit cancellation and acknowledged hold release before the
original owner settles/top-ups through normal Billing and retries a new transfer.
There is no top-up or saved-card charging exception to the handoff monetary fence.

After settlement, both parties confirm the same versioned balance snapshot.
Positive balance stays in the account and ledger unchanged; changed amounts
require reconfirmation. Make old payment methods/consents unavailable before
ownership commit. Finalize with the committed Account Manager ownership version,
close/open responsibility periods, permanently revoke old auto-charge consent
and return a durable acknowledgment. The new owner supplies new payment consent.
Never duplicate balance/debits or expose old provider payment references.
Before exposing the new owner's Billing view, retire the prospective billing
profile into restricted historical evidence and replace its active projection
with an unset profile tied to the new ownership version. Do not copy old legal
name, tax ID, address, email, invoice-delivery destination or profile defaults.
New owner supplies and confirms its own profile before new invoice issuance or
delivery that needs a recipient; accounting continues without reusing old PII.
Historical invoice recipient snapshots remain immutable. Prepare hides the old
profile from the target; finalize resets it idempotently before releasing access.

Abort requires confirmed Account Manager precommit cancellation. Restore only
still-valid, not externally revoked consent through an audited transition. After
owner commit keep fences and retry finalization; never restore old-owner access
on timeout. Provider callbacks remain enabled for reconciliation; unexpected
balance changes keep the operation blocked. Recovery uses persisted versions.

After a finalized transfer, compensation for an original predecessor transaction
(including later refunds/chargebacks) is recorded in a separate predecessor-period
adjustment ledger/receivable, never against the new owner's spendable cloud balance.
Key entries by provider event, original transaction and proven responsibility period;
do not overwrite original cloud ledger history or automatically charge an old method.
The platform reconciles provider cash movements and recovery separately. Unknown
liability goes to audited exception handling without defaulting to the current owner.
This preserves the confirmed opening balance and privacy policy even after deletion.
Test delayed setup completion across prepare/commit/finalize, and duplicate/reordered
post-finalization chargebacks/refunds: old methods remain unusable, new-owner balance
and historical snapshots unchanged, compensation appears once in the correct ledger.

## Internal HTTP transport

### New-cloud responsibility provisioning

Tenant account reads, internal debit ingestion and period closing must not
implicitly create commercial accounts. Debit and close-period requests for an
unprovisioned cloud return retryable `503 BILLING_ACCOUNT_NOT_READY` without
account, invoice or ledger writes. Retry the original request after the creation
event commits; this prevents ordinary billing work from racing ahead of the
initial owner evidence. Existing accounts still require the reviewed history
migration; these routes never infer or overwrite their ownership.

AM's new-cloud transaction persists the cloud UUID, initial unique global owner,
version 1, creation time and immutable event UUID. Migration 066 emits this event
only for newly inserted Brand Clouds after the owner transaction is complete; it
does not scan or attribute legacy clouds. Pending activation does not remove the
designated owner, but normal authentication/operational checks still prohibit use.

The optional dedicated `BILLING_CLOUD_CREATION_TOKEN` accepts these events at
`POST /v1/internal/billing/cloud-creations`, separate from handoff/tenant/internal/
debit/provider authority. Migration 057 stores append-only creation receipts.
Account, initial responsibility, receipt and audit commit atomically; an existing
account without the exact receipt is rejected even if its balance is zero. No
funds, payment consent, legal profile or access override is provisioned.

AM retries the same event after timeout or worker restart. Only the full matching
receipt is acknowledged under the current delivery lease. Billing's replay reads
the original receipt and never reopens a closed account, resets a balance, or
overwrites later ownership periods. Changed owner/time/event/cloud/digest conflicts.
This path cannot bootstrap existing historical accounts; the reviewed provenance
and migration workflow remains necessary. It also supplies no usage/provider
completeness or financial eligibility evidence.

### Advisory request/acceptance eligibility

Usage replay requires exact equality of every immutable fact field, not just a
caller-supplied source hash. The receipt binds cloud, service, metric, quantity,
scale, unit, window and source. Source hashes must be hexadecimal SHA-256 values;
UUIDs and UTC microsecond timestamps are normalized before insertion/replay.
Forward migration 058 prevents rewriting/deleting accepted usage facts without
changing earlier migration markers or historical values. Corrections require
new auditable facts. This integrity gate is not producer completeness: source
drain, durable forwarding, lateness and provider/invoice reconciliation still
need independent collector proof before ownership can change.

`POST /v1/internal/billing/clouds/{orgId}/ownership-eligibility` is a read-only
financial query under the same dedicated handoff credential and 15-second
deadline. Headers bind `X-Billing-Owner-User-ID` and the canonical positive
`X-Billing-Ownership-Version`; JSON supplies `target_user_id`, `action` (`request`
or `accept`) and `transfer_id` (absent/empty for request, mandatory UUID for
acceptance). Source and target must differ. AM independently authenticates both
identities and validates its persisted transfer; Billing does not own user records.

The response echoes the complete request binding and returns `complete`,
`receipt_id`, `evidence_sha256`, currency, signed minor-unit balance, stable
blockers, observation time and expiry. Only current responsibility and a fresh,
local-state-matching collector receipt can provide complete evidence. The evidence
digest binds that receipt to the action, target and transfer ID. Missing/expired
evidence returns `complete=false` with `evidence_unavailable`; it never becomes
approval just because local tables are empty. Negative balance returns
`balance_negative`; zero and positive credit remain subject to every independent
financial and lifecycle blocker. This does not reuse deletion's zero-only rule.

The query does not create an account, hold, receipt, consent or ownership period.
It exposes no payment/invoice/provider or predecessor PII. AM checks echo, bounded
lifetime and completeness before using the response. Later fenced settlement and
commit revalidate everything; this endpoint neither reserves credit nor certifies
producer drain. Collector ingestion remains a separate trusted workflow, with no
coordinator endpoint for manufacturing evidence. Production admission stays
disabled until the actual participant inventory and collectors are installed.

The Billing implementation exposes the following optional coordinator operations
under `/v1/internal/billing/clouds/{orgId}/ownership-handoffs/{operationId}`:

| Method / suffix | Authority and result |
| --- | --- |
| POST `/prepare` | Source/target/cutoff from AM's persisted acceptance. Installs a hold, not settled readiness. |
| GET `/settlement` | Live validity, minimal amount/version snapshot and blockers. No invoice, payment-method or predecessor identity details. |
| POST `/confirm` | AM-authenticated source/target confirms exact amount, currency and snapshot. Billing independently checks participant and persisted live evidence. |
| POST `/authorize-commit` | Returns a durable grant only with both current confirmations and matching settlement. AM must also verify every producer's hold/drain evidence. |
| POST `/finalize` | Verified durable AM commit, grant ID and committed boundary. A known commit remains recorded even when local finalization fails. |
| POST `/abort` | Verified precommit AM cancellation. Returns `abort_pending`; delivery alone does not release Billing's hold. |

Every operation requires the dedicated `BILLING_HANDOFF_TOKEN` plus the original
`X-Billing-Ownership-Version`. Responses echo cloud/operation/version and carry
`Cache-Control: no-store`. Request JSON is bounded to 16 KiB, rejects unknown
fields/trailing documents, and all handlers have a 15-second context deadline.
Identical retries reuse the durable IDs/payload; unexpected or unavailable evidence
never becomes approval. Tenant, pricing/access, debit and provider credentials
cannot enter this group; the handoff credential cannot enter those other groups.
No handoff route exists unless the dedicated runtime is configured before serving.

There is deliberately no coordinator route for responsibility bootstrap,
settlement-receipt ingestion or hold-release certification. Those require separate
trusted provisioning/collector/provider workflows. An emailed token or browser
confirmation is not one of these proofs. Production coordinator wiring, verified
producer inventory/checkpoints and global-session enforcement remain release gates.

The AM transport uses HTTPS (literal loopback HTTP is allowed only for isolated
tests), refuses redirects, bounds responses and validates the echoed scope and
nested snapshot/grant/acknowledgment. It does not autonomously retry, change an
operation ID or infer a successful commit from a timeout. Durable workers own retry.

### Cross-repository transport verification

`TestHandoffAccountManagerClientContract` optionally receives
`ACCOUNT_MANAGER_HANDOFF_CLIENT_DIR` pointing to the isolated AM implementation
checkout. It starts a loopback Billing router with isolated PostgreSQL fixtures,
then invokes AM's `TestLiveBillingTransportContract` against that HTTP server.
This proves the actual independently compiled client/server serialization and
Billing persistence, not an AM membership commit, browser session or real provider
settlement. The test never targets staging; its AM side rejects non-loopback URLs.

## Deletion and evidence

Deletion preparation requires zero balance, settled usage and no pending monetary
or provider work. Account Manager separately proves empty resources/jobs. Close
Billing access idempotently before cloud tombstoning; retain ledger, invoices,
consents and responsibility history. No cascade delete.

Simulator tests cover delayed/duplicate callbacks, in-flight payments/refunds,
disputes, stale confirmations, credential separation, every crash boundary,
old-owner denial, balance preservation, historical attribution and deletion.
Cover balances -1/0/+1, positive credit with unsettled invoices or pending work,
nonnegative-to-negative cutoff races, and cancellation/settlement/retry. Both 0
and +1 qualify only when all other financial checks pass. Positive-to-zero
changes require fresh amount confirmation. Deletion still requires exactly zero.
Include predecessor invoice IDs, downloads, exports, mixed-period statements,
returning owners and pagination counts in financial-privacy negative tests.
Also assert that ledger details, payment-intent lists/details/attempts and even
redacted payment-method metadata cannot expose a predecessor period. Exercise
historical and overlapping-month usage requests, summary `current_period`, totals
and forecasts, plus profile reset, stale profile ETags, and invoice delivery that
must not address the predecessor. Both direct reads and aggregates enforce the
same period filter before any calculation. Exercise
participant-only preview/confirmation before commit, denied target general Billing
reads, stale/expired participants, and two tabs with hosted-return scope changes.
Coordinate matched backups/restore under write freeze. Database restore cannot
undo external payments/consents: reconcile provider outcomes/idempotency keys
with charging disabled before workers resume. Never automatically replay a charge
because its local completion record was rolled back. Staging evidence is required.

Use the workspace [Core Backup and Restore](../../../docs/backup-restore.md)
procedure for the matched maintenance-window set, target safety backup and
explicit verify/resume gates. Billing PostgreSQL is included; an archive cannot
roll back payment-provider state, consent or previously sent email. Keep
dispatch disabled while reconciling these effects, including when restoring
after a fresh deployment. A `scope=core` archive is not full-environment recovery.
