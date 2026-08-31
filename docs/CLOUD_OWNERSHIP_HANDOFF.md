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

## Durable financial boundary

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

After settlement, both parties confirm the same versioned balance snapshot.
Positive balance stays in the account and ledger unchanged; changed amounts
require reconfirmation. Make old payment methods/consents unavailable before
ownership commit. Finalize with the committed Account Manager ownership version,
close/open responsibility periods, permanently revoke old auto-charge consent
and return a durable acknowledgment. The new owner supplies new payment consent.
Never duplicate balance/debits or expose old provider payment references.

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
Coordinate matched backups/restore under write freeze. Database restore cannot
undo external payments/consents: reconcile provider outcomes/idempotency keys
with charging disabled before workers resume. Never automatically replay a charge
because its local completion record was rolled back. Staging evidence is required.
