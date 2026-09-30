# OTA Pricing Activation Runbook

Status: reviewed publication tooling exists in code; **do not publish an OTA rate card before environment qualification, migration review, direct-object delivery cost review and an approved future UTC month**. CDN is a later expansion.

Owner: Billing and Finance. Last reviewed: 2026-09-27.

Use this with the [OTA billing design](ota-device-task-billing.md), the
[pricing and invoicing contract](../../rtk_cloud_contracts_doc/pricing_and_invoicing.md),
the [OTA delivery contract](../../rtk_cloud_contracts_doc/ota_delivery_and_billing.md),
and the workspace [activation plan](../../../docs/design/ota-pricing-activation-and-disclosure-plan.md).
The workspace [staging qualification runbook](../../../docs/billing-staging-qualification.md)
governs any live staging run. This document does not authorize staging or
production mutations.

## Current boundary

Four OTA unit prices have been **approved before tax but remain inactive**.
Their exact values are recorded in the canonical OTA contract and the current
planning helper; the future reviewed manifest must match both. This public
operating document does not duplicate a customer price list.

| `service_code=ota` metric | Fact unit | Fact quantity scale | Billable event |
| --- | --- | ---: | --- |
| `device_task` | `tasks` | 0 | First durable campaign/device assignment |
| `successful_download_gib` | `GiB` | 9 | First accepted authenticated download report with matching grant |
| `artifact_storage_gib_month` | `GiB-month` | 9 | Physical artifact byte-seconds integrated over the UTC month |
| `artifact_write` | `requests` | 0 | Successful artifact object creation |

`internal/billing/ota.go` supplies these planning values but does not install
them. A Product's OTA grant permits the service; it does not make a rate
effective. Only the Billing version applicable to an invoice period can price
facts. `internal/billingstore/pricing.go` currently permits a **draft** with OTA
rates but rejects activation of every OTA-containing version through the
generic `/v1/internal/billing/pricing-versions/{id}/activate` route. The
separate `publish-reviewed-ota` route checks the current complete base, the
exact four OTA rates, deterministic digest, two distinct reviewers, 5%
invoice-total tax, Product/account scope and a future UTC month boundary in
one serialized transaction. It requires a complete draft assembled by
`cmd/ota-pricing-draft` with its digest in `ota_pricing_drafts`, then records
the publication in `ota_pricing_publications`. Do not bypass these checks
through SQL. No OTA
retail version has been activated by this runbook.
For paid Managed Cloud, a Product with the `ota` option and a qualifying source
receipt is the OTA charge boundary; there is no separate OTA contract opt-in.
After disable, authorized prior work may complete and existing artifact bytes
remain billable until physical deletion. A Product ID without verifiable
historical grant evidence is insufficient. OTA is not tax-exempt: all service
line subtotals are combined before applying the reviewed invoice tax policy
once. The approved policy for the new OTA-inclusive TWD card is Taiwan business tax at 5% (500 basis points), `invoice_total`, and `half_up` rounding on the combined pre-tax subtotal. OTA has neither a separate surcharge nor an exemption. Government electronic-invoice issuance and filing are outside this phase; internal Billing invoice records remain.

The internal usage-fact API can preserve the original Product grant revision,
digest and authorization time for OTA task, verified-download and artifact-write
facts in immutable columns. These fields are optional during source rollout.
If a reviewed OTA rate card is later activated, invoice close requires an
Account Manager historical lookup for each distinct grant, matching the
Product, enabled revision, digest and authorization interval. Missing client
configuration, missing witness or a failed lookup leaves the period incomplete
with `ota_grant_unverified`. For storage, a new source fact represents one
physical object in one UTC month. Its immutable object-key digest and exact
byte-microseconds accompany the original grant; Billing checks uniqueness and
recomputes each Product's rounded monthly total from those contributions.
A positive legacy Product-month storage aggregate without per-object evidence
cannot be charged. The current usage preview applies the same check; it
withholds OTA lines and the full-bill forecast when evidence cannot be
verified. Publication remains subject to environment qualification and the approved future month.

Before pricing exists, accepted OTA facts remain immutable evidence while
`BillableUsageFacts` excludes them from invoices and estimated charges. A
mixed-service month still needs priced non-OTA facts; a missing non-OTA rate or
partial OTA meter set fails closed. The usage API's `fact_count` and
`usage_through` refer to facts included in its priced estimate, not every
accepted source fact. A receipt rejected after the period's closing barrier
must remain in the producer ledger/outbox with its payload and rejection
reason; Billing cannot silently move it to a later month.

## 1. Record a read-only environment inventory

Do this separately for each target environment with approved read-only access.
Record the environment, timestamp in UTC, code/image digests, database/schema
revision, operator, and query or export digest. Never infer a production card
from staging, a local helper, or Cloud Admin's reference-price page.

Use one consistent **read-only** database snapshot to capture:

1. Every TWD pricing version's ID, `plan_key`, version, status, effective UTC
   interval, `activated_at`, and creator. Identify the version selected by
   `ActivePricingVersion(period_start, TWD)` for each open and recently closed
   invoice month, plus every scheduled or draft candidate. A missing or
   overlapping selected interval is a stop condition.
2. The selected version's **entire** `pricing_rates` set: service, metric, unit,
   description, price minor/scale, rounding, tax basis points, and rate ID.
   Include non-OTA services, not only the four proposed OTA rows. Compare with
   the rate and `pricing_version_id` snapshots of issued invoices; never edit
   issued invoice lines.
3. Current account/contract categories, evaluation and private deployments,
   ownership transfers, billing profiles and period timezones, existing
   invoices/closing periods, source fact counts, OTA outbox and receipt
   high-water, object inventory, and both period-seal producers. Store only
   scoped aggregates and redacted IDs in the approval packet.

The current `ActivePricingVersion` lookup uses time and currency, **not**
`plan_key`, account, or contract. Rate rows now have nullable `tax_category`
and `quantity_scale` metadata; historical rows remain null until reviewed.
A zero `tax_rate_basis_points` is a default, **not** evidence of an approved
tax exemption. The inventory must expose unresolved values and scope gaps,
not label the existing implementation a valid customer-specific preflight.

### First TWD card in development

Development had no price-book version or invoice on 2026-09-30. The owner
approved using the highest researched non-OTA numbers as **development test
rates**. The complete candidate is
`cloud_env/dev/pricing-initial-rates.json` in the workspace: 11 priced meters
plus MQTT publish/delivery bytes at zero because the bytes are diagnostic
facts and carry no separate bandwidth fee. The two MQTT count meters have an
operational source; the other nine priced meters remain dormant until their
source facts, units, Product scope and close checks are qualified. An installed
rate alone never creates a usage fact or invoice. This approval is limited to
development and does not approve production rates.

Use `go run ./cmd/initial-pricing-review --candidate
/absolute/workspace/cloud_env/dev/pricing-initial-rates.json` from the Billing
repository to compute the deterministic complete-rate digest. Review every
identity, unit, precision and amount against the candidate and the dated
research ledger; the command is offline and does not publish. Create a draft
through the authenticated internal pricing API with this exact `rates` array,
`currency=TWD`, `tax_mode=invoice_total`,
`invoice_tax_rate_basis_points=500`, `invoice_tax_rounding_mode=half_up`, and
`invoice_tax_category=standard`. Choose `effective_from` as the first instant
of the **next** UTC month. Billing publishes the reviewed first card as
upcoming; it becomes current at that boundary, and no prior month can be
priced retroactively. Its per-rate
`tax_rate_basis_points=0` is metadata for the invoice-total mode, not a tax
exemption.

The generic `/activate` route deliberately rejects invoice-total drafts.
Publish the initial complete non-OTA card through
`POST /v1/internal/billing/pricing-versions/{id}/publish-reviewed-initial`
with the reviewed `rate_set_sha256`, a real `approval_reference`, and
`approved_at`. In one serialized transaction Billing requires the fixed Taiwan
5% invoice-total policy, complete standard-tax rate metadata, exact digest,
no OTA rows, no prior active/retired card, and no existing billing period or
invoice. The immutable `reviewed_initial_pricing_publications` row records
the digest and approval. A failed preflight changes nothing. Do not use SQL to
bypass this route. Recheck the saved card and the authenticated Cloud Admin
effective-price view after publication. OTA remains absent from this first
card and cannot be charged until its own future-month reviewed publication.
`cmd/ota-pricing-review` now provides a **read-only technical comparison** of
one complete candidate against the current TWD version in one repeatable-read
snapshot. It does not collect every version, contract, invoice, source ledger
or approval, and its digest is not a signed approval packet. The broader
inventory above remains required and cannot by itself authorize activation.

For a proposed future UTC month, run the read-only cutover inventory against
each target environment with approved database access:

```sh
DATABASE_URL='read-only connection string' go run ./cmd/ota-cutover-audit \
  --effective-from "$PROPOSED_OTA_UTC_MONTH_START" > ota-cutover-audit.json
```

Set `PROPOSED_OTA_UTC_MONTH_START` to a future first-of-month RFC3339 UTC
timestamp before running the example.

The command uses one repeatable-read, read-only snapshot and emits SHA-256
organization references, account state, profile timezone, the old local-month
boundary, and the interval between it and the proposed UTC boundary.
`gap_risk` means the local boundary precedes UTC midnight; `overlap_risk`
means it follows. These names describe a potential cutover problem, not proof
that a fact was omitted or billed twice; the actual period and invoice counts
must be reviewed before choosing a bridge treatment.
It also counts usage facts and existing closed periods/invoices touching that
bridge, any periods/invoices touching the first UTC month, and whether current
owner/profile evidence covers the cutover. Missing profiles remain explicit.
The output is **technical inventory only**: it does not choose who pays for
bridge usage, split an issued invoice, prove source completeness, or permit
rate publication. Preserve the JSON digest with the environment inventory and
resolve every gap, overlap, ownership exception, and existing target-month
financial record before selecting a migration procedure.

Invoice close rejects a proposed period that overlaps any different existing
period for the same Brand Cloud and currency, including an incomplete period.
The exact same period remains an idempotent retry. This prevents a local-month
bridge and a UTC month from both creating financial records for the same time;
it does not fill a gap or decide the bridge allocation. Resolve conflicts in
the reviewed migration procedure rather than changing issued invoices.

After a reviewed OTA card has been published, close the final profile-local
month with `cmd/ota-cutover-bridge`. It keeps that month's original local start,
extends or truncates its end to the first OTA UTC month, and prices the entire
old period with the preceding complete card and its original tax policy. OTA
facts from this period remain as evidence without a charge. The ordinary close
command holds an overlapping, not-yet-issued old period so that an operator
cannot accidentally omit or duplicate the local/UTC boundary hours.

```sh
DATABASE_URL="$READ_ONLY_BILLING_DSN" go run ./cmd/ota-cutover-bridge \
  --organization "$BRAND_CLOUD_ID" --pricing-version "$PUBLISHED_OTA_VERSION_ID" \
  > reviewed-ota-bridge.json

DATABASE_URL="$WRITE_BILLING_DSN" go run ./cmd/ota-cutover-bridge \
  --organization "$BRAND_CLOUD_ID" --pricing-version "$PUBLISHED_OTA_VERSION_ID" \
  --apply --review-sha256 "$REVIEW_SHA256" --created-by "$OPERATOR_ID"
```

The first command uses one read-only consistent snapshot and returns the old
period, previous rate version, fact count, subtotal, tax, total and a digest.
Review the actual account, owner, profile, pricing, facts and overlapping
periods before applying. The second command rechecks that exact digest under
the pricing/account transaction locks and records an immutable receipt with
the invoice. It holds before the UTC boundary, when the owner/profile is
incomplete, when a fact crosses the boundary, or when anything changed since
review. Use a fresh read-only review after a hold. Repeating a successful
apply with the same digest returns the original invoice. Do not run this
against a live environment until its OTA source and pricing qualification has
passed; a zero-fact old period remains held for review.

## 2. Resolve commercial and data-model prerequisites

Stop before constructing a publishable draft unless the decision record
contains all of the following:

| Decision / prerequisite | Required evidence and implementation |
| --- | --- |
| Tax | The approved new OTA-inclusive TWD card uses `invoice_total`, Taiwan standard business tax at 5% (500 basis points), `half_up`, and the `standard` category: round each pre-tax line, sum all services, calculate tax once on that subtotal, then allocate tax to lines for reconciliation. Government electronic-invoice issuance and filing are deferred. Existing issued invoices and prior `line` versions retain their old meaning. `tax_rate_basis_points=0` on an OTA rate is not an exemption. Reconfirm this frozen policy in the approved manifest before publication. |
| Applicability | Paid Managed Cloud eligibility is Account Manager `commercial_for_full_period=true` for the complete UTC month plus `commercial_accounts.state=active` at settlement. No extra contract/plan marker is required. Each charged OTA fact also needs a verified Product grant and qualifying source receipt, including bounded completion and storage after disable. The invoice close and usage estimate checks are implemented; the tenant price API must apply the same effective interval and qualify its display. Evaluation and Private Cloud retain their separate terms. Missing or ambiguous evidence fails closed. |
| Meter precision | Require a reviewed, non-null rate `quantity_scale` on the publishable full card and the four exact OTA service/metric/unit/price/rounding identities. The nullable rate field now round-trips and rejects mismatched facts when set; historical nulls still need explicit review. |
| Version provenance | The read-only review tool checks the selected base ID and produces a deterministic rate-set digest. The atomic draft command copies the current complete base and adds exactly four approved OTA rows in one locked transaction, recording scope, tax and digest in `ota_pricing_drafts`. The reviewed publication transaction rechecks that manifest and the then-current base against the approved digest, and records reviewers and cutover in `ota_pricing_publications`. The operator must preserve the external approval packet. |
| UTC cutover | The generic activation path permits one future `00:00:00Z` first-of-month **non-OTA** version. The separate reviewed OTA path applies the same serialized non-overlap and invoice protections, additionally requiring the frozen full-card manifest and tax policy. It marks the prior row `retired` when published, but interval selection continues to use it until the cutover. Operational publication still requires target-environment qualification; preserve historical intervals and invoices. |
| Ownership/month policy | OTA-priced invoice close requires an exact UTC month and a current responsibility period that began no later than that month's start. Missing ownership evidence, a mid-month transfer, or Cloud closure leaves an explicit `incomplete` period for manual review. The current tenant preview now uses the UTC month once OTA is priced and withholds OTA estimates on partial/current-owner windows while retaining other service estimates; it reports `held_for_review` and disables the full-bill forecast. Still specify migration from historical profile-local months and any approved allocation or later-owner close policy without exposing another owner's data. |

The four approved prices do not answer these questions. Finance must also
review the real CDN cost and margin: the approved charge for a **verified
logical GiB** may be below a raw-egress research proxy in some regions.
CDN bytes, retries and Range requests are provider costs, not an extra
customer meter. Use the actual provider contract, regions, tiers, cache
behavior, retry/Range ratio, exchange rate and tax in the margin record.

## 3. Build and review the complete candidate card

Sort rate identities deterministically by
`(service_code, metric_code, unit)` and produce a machine-readable full-card
diff and digest. The candidate must copy **every** applicable non-OTA row
unchanged, then add exactly the four OTA rows above. Review description,
price minor/scale, expected fact scale, rounding, invoice tax mode/category/rate, scope,
currency TWD and UTC interval. A per-Product OTA grant is not a substitute
for paid Managed Cloud account eligibility. Do not promote Cloud Admin reference prices
or the other unapproved service benchmarks into this card.

The first technical check is available from the Billing repository. Provide a
protected JSON file containing the **complete** candidate `pricing_rates`
array, including explicit quantity precision and tax metadata on every row:

```bash
DATABASE_URL="$READ_ONLY_BILLING_DSN" go run ./cmd/ota-pricing-review \
  --base-version "$CURRENT_TWD_VERSION_ID" \
  --effective-from "YYYY-MM-01T00:00:00Z" \
  --candidate /protected/path/complete-rates.json
```

The command runs a read-only repeatable-read transaction, verifies the base
is current and no published future card exists, checks that existing monetary
terms and known metadata are unchanged and the four approved OTA meter
identities are exact, then emits
the selected base and a deterministic candidate rate-set SHA-256. It does
**not** prove Finance approved the supplied tax category/rate, that the
account scope is implemented, or that the base will still be current when a
draft is later created. Store its output with restricted approval evidence;
it contains the full internal price card. A failed review leaves the database
unchanged.

For a legacy base with null `quantity_scale` or `tax_category`, the candidate
must explicitly fill every missing field. The review permits only those null
fields to be completed; it never changes a known field, rate identity, unit,
description, price, rounding mode or recorded tax basis points. Check fact
precision and the approved tax treatment for **each** service, including test
or qualification services, before signing off. The candidate and digest are
the approval artifact; never backfill the historical base or issued invoices.

After the technical review and external Finance/Billing scope and tax review,
create the unpublished card with a write-authorized Billing connection:

```bash
DATABASE_URL="$WRITE_BILLING_DSN" go run ./cmd/ota-pricing-draft \
  --base-version "$CURRENT_TWD_VERSION_ID" \
  --effective-from "YYYY-MM-01T00:00:00Z" \
  --candidate /protected/path/complete-rates.json \
  --review-sha256 "$REVIEWED_RATE_SET_SHA256" \
  --created-by "$OPERATOR_ID"
```

In a serialized transaction the command re-reads the current complete TWD
card, requires the supplied base ID and a future UTC month start, rejects
another published future card, checks the supplied full candidate against
every base rate and the four approved OTA rates, and requires its digest to
match the read-only review. It fixes invoice-total Taiwan tax at 5% and writes
the draft plus its immutable manifest digest. The candidate can only fill
missing legacy precision and tax category; all non-OTA monetary terms remain
unchanged. Compare the returned digest and full rate card with the approved
review packet. Creation does **not** activate or publish prices. The command
also supports omitting `--candidate` and `--review-sha256` when the base card
already has complete reviewed metadata; do not use that shortcut for a legacy
base with null fields.

Reject the candidate if any non-OTA row changes or disappears without its own
commercial approval, an OTA row is missing/duplicated/altered, another future
version conflicts, tax/scope is unresolved, the base has drifted, or an
issued/closing period intersects the proposed cutover. The reviewed publication
transaction revalidates the atomic draft against the live base. The generic
`POST /v1/internal/billing/pricing-versions` only creates a draft. It is
**not** the preflight or publication mechanism; the operator must use the
read-only review and the separate reviewed publication route.

Keep in the audit packet: redacted inventory, old and candidate full-card
manifests and digest, rate-by-rate diff, all Finance/Legal and Billing
approvals, applicable contracts, effective UTC month, staging evidence links,
and the eventual immutable draft/version IDs. Research references belong in
the Cloud Admin research document, separately from effective prices.

## 4. Qualify the source and customer view in staging (future Q1)

Use an authorized isolated staging run, fixed image and candidate version,
and fresh test Cloud/Products. The existing payment [qualification runbook](../../../docs/billing-staging-qualification.md)
does **not** by itself prove OTA delivery or billing. Record passing evidence
for each row before proposing production publication:

| Gate | Evidence |
| --- | --- |
| Product/service | OTA registration, Product enable/disable, historical grant revisions, denial of new work after disable, and the dashboard's unavailable state for Products without OTA. |
| CDN/object path | Signed private-origin CDN URL, direct device download, HTTP Range resume, token expiry/revocation bound, object SHA/size, first durable assignment, matching grant and authenticated `downloaded` receipt. Confirm URL issuance, CDN GETs, partial/failed transfers and retries create no extra customer download charge. |
| Four source meters | First assignment, first verified download, actual physical byte-seconds through deletion, and successful object creation each have immutable receipts, idempotent Billing facts, correct Product and UTC window, quantities and source digests. Reconcile raw CDN egress separately as provider cost/anomaly evidence. |
| Completeness | Platform `platform_grants` seal covers every historically authorized Product; producer `ota_producer` seal covers facts/objects and all four metric counts. Compare source high-water and delivered outbox with Billing fact IDs/content; verify exact Product sets and fact SHA-256. Include zero-use, disabled and retired Products. Missing/mismatched seals, unknown objects and unexplained CDN anomalies must leave the close `incomplete`. |
| Price and period | Verify full UTC-month invoice/estimate, both sides of the future cutover, other services on the same card, pre-tax per-Product line rounding, one tax calculation on the combined invoice subtotal and deterministic line allocation, owner transfer/closure holds, old months with OTA evidence but no OTA charge, and unchanged issued invoices. Test the actual signed-in current/upcoming price API and its account scope when A1 exists; the current reference page is not proof of an effective card. |

`internal/billingstore/invoices.go` rejects an OTA-priced close outside a
complete UTC month or without a current owner covering the full month,
before checking source seals. After source verification it also requires the
billing profile's ownership version to match that current responsibility.
It retains the attempted period with `ota_period_not_utc_month`,
`ota_ownership_month_incomplete`, or `ota_ownership_profile_mismatch`; historical
non-OTA months keep their existing close path. This is a conservative hold,
not an ownership allocation or an approved historical-period migration.

`internal/api/billing.go` now selects a UTC month for the tenant's current
usage when that month's card includes OTA. A partial or new-owner window
excludes OTA facts from the estimate, keeps other priced services visible,
returns `ota_estimate_status=held_for_review` with a reason, and suppresses
the full-bill forecast. A complete current-owner UTC month may show an OTA
estimate, which remains provisional until dual seals and close succeed.

`internal/billingstore/ota_period_seals.go` already stores immutable,
digest-idempotent seals and compares Product sets, counts and fact digest when
an OTA-priced period closes. That local behavior does not prove the producers
ran in a protected environment or that source high-water and CDN anomalies
were operationally reconciled. Preserve the run ID, pinned versions, sanitized
receipts, two seals, producer/Billing high-water comparison, CDN report,
invoice calculations, failure cases and reviewer sign-off. Do not dispatch a
shared staging run from this document alone.

## 5. Publish only a qualified future month (future P2/R1)

This section is the acceptance sequence for the reviewed publication tooling.
It is not permission to activate a card in an unqualified environment.

1. Finance, Billing and operations recheck the approval packet and select the
   first **not-yet-started full UTC month** allowed by notice and contract
   terms. Record the precise `YYYY-MM-01T00:00:00Z` boundary. No past or
   in-progress month may be back-priced.
2. Re-run the read-only complete-card and account-scope diff against the
   target environment. Compare the current base, all pending versions and
   invoice/period states with the approved manifest digest. A mismatch aborts
   the publication before any write.
3. Use `POST /v1/internal/billing/pricing-versions/{id}/publish-reviewed-ota`
   with the approved base ID, rate-set SHA-256, two distinct reviewers and
   approval time. The transactional publisher rechecks the full draft, stores
   the audit row and schedules its effective interval. Re-read the
   database and tenant current/upcoming API: **before** the boundary the old
   version must still price the month; the new version must be merely
   upcoming. Verify authenticated Cloud Admin wording, notice and tax.
4. At the UTC boundary, verify the selected version, account applicability,
   current API and price display. Keep receipts/outbox flowing. Do not close
   the first OTA month until the complete UTC month, dual seals, source/CDN
   reconciliation and owner-allocation gates pass.
5. Reconcile the first issued OTA invoice by organization, Product and each of
   the four meters: source receipt IDs/digests and accepted window → Billing
   immutable facts → line `quantity`/`quantity_scale` and rate ID/version →
   subtotal, tax and total. `BuildDraftInvoice` aggregates each Product/meter
   before `PriceUsage` rounds TWD minor units once per line; invoice snapshots
   and `usage_fact_refs` must match. Check mixed non-OTA lines and the prior
   month's unchanged invoice. Record approvals of any zero-use or held month.

The first-month report must include the environment/image/database version,
UTC cutover, customer notice, old/new manifest digests, selected version IDs,
two seals and high-water, CDN cost/anomalies, period close result, invoice/PDF
and line-level reconciliation, and approvers. Redact customer and credential
data. Publish the report's location and immutable CI/run links in the handoff.

## Abort, correction and no-retrocharge rules

| When a gate fails | Required response |
| --- | --- |
| Before draft/publication | Stop; retain the read-only inventory and failed diff. Correct the model or manifest, obtain fresh approval, and re-run qualification. Do not create a partial OTA-only card. |
| After scheduling but before the cutover | The reviewed cancellation API below atomically marks the future card `canceled`, restores the prior card's open interval, and retains both approval records. Recheck current and upcoming price APIs. A replacement needs a fresh complete draft, review digest, and publication; never edit or delete the old rows. |
| After the cutover, before invoice issue | Hold affected period close as incomplete and continue preserving receipts/outbox. Diagnose source, dual seals, CDN anomalies, scope/tax and rate selection. A corrective version may apply only from a later approved full UTC month. |
| After invoice issue | Preserve the immutable invoice and its rate/fact snapshots. Use the approved adjustment/credit and customer-notice process when implemented; do not rewrite the invoice, move old facts to another month, or backdate a new OTA rate. |

For a published card whose UTC cutover is still in the future, obtain a
documented cancellation reason and two distinct reviewers, then call
`POST /v1/internal/billing/pricing-versions/{id}/cancel-reviewed-ota` with
`base_version_id`, `first_reviewer`, `second_reviewer`, `reason`, and
`approved_at`. This internal-token endpoint rejects a cutover that has begun,
a mismatched base, an already canceled card, or a financial period extending
beyond the scheduled boundary. It shares the price-publication lock with invoice
close, so the check and interval restoration cannot race a close. Preserve the
`ota_pricing_cancellations` row with the original publication record, then
verify that the prior version is selected on both sides of the former boundary
and no upcoming OTA card appears. The audit fields record supplied reviewer
identifiers; the external approval packet remains the authority for reviewer
identity and authorization.

Pricing selection uses the version interval at the invoice **period start**.
Facts from a preactivation month remain audit evidence and are never later
included by rebuilding that month with a new card. A late report accepted in
the first priced month belongs to its server-acceptance UTC window under the
contract; a fact rejected by a closed-period barrier stays in the source
ledger for explicit reconciliation. An operational rollback cannot erase a
customer's usage evidence or make an already issued invoice mutable.
