# Brand Cloud Billing ownership handoff design

Status: design-first target, not deployed acceptance. Follow canonical
MULTICLOUD_OWNERSHIP.md. Account Manager owns membership/ownership; Billing owns
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
