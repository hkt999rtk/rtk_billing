# OTA Pricing Activation Runbook

Status: design-stage operating procedure; **do not use it to publish an OTA rate card yet**.

Owner: Billing and Finance. Last reviewed: 2026-09-26.

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
immediate `/v1/internal/billing/pricing-versions/{id}/activate` route. Do not
bypass this protection through SQL or another internal endpoint. No OTA retail
version has been activated by this runbook. The generic activation route can
schedule a **non-OTA** card for a future UTC month boundary and serializes its
publication with invoice close; it still rejects OTA rates until the complete
approval, scope, tax, and month rules below are implemented.
For paid Managed Cloud, a Product with the `ota` option and a qualifying source
receipt is the OTA charge boundary; there is no separate OTA contract opt-in.
After disable, authorized prior work may complete and existing artifact bytes
remain billable until physical deletion. A Product ID without verifiable
historical grant evidence is insufficient. OTA is not tax-exempt: all service
line subtotals are combined before applying the reviewed invoice tax policy
once. The actual invoice tax rate and formal invoice treatment remain pending.

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

## 2. Resolve commercial and data-model prerequisites

Stop before constructing a publishable draft unless the decision record
contains all of the following:

| Decision / prerequisite | Required evidence and implementation |
| --- | --- |
| Tax | Finance/Legal sign-off for the invoice tax rate, category, rounding and formal invoice treatment. New OTA pricing uses `invoice_total`: round each pre-tax line, sum all services, calculate tax once on that subtotal, then allocate tax to lines for reconciliation. Existing issued invoices and prior `line` versions retain their old meaning. `tax_rate_basis_points=0` on an OTA rate is not an exemption or approval. |
| Applicability | Paid Managed Cloud Products with a verified OTA grant and qualifying source receipt are eligible, including bounded completion and storage after disable. Implement that evidence check in invoice close, usage estimates, and the tenant price API with the same effective interval. Evaluation and Private Cloud retain their separate commercial terms. Missing or ambiguous assignment must fail closed. |
| Meter precision | Require a reviewed, non-null rate `quantity_scale` on the publishable full card and the four exact OTA service/metric/unit/price/rounding identities. The nullable rate field now round-trips and rejects mismatched facts when set; historical nulls still need explicit review. |
| Version provenance | The read-only review tool checks the selected base ID and produces a deterministic rate-set digest. Still persist the approved base identity, scope, manifest digest, approvers and approval timestamp; draft creation must recheck them atomically. No approved draft/publish transaction exists yet. |
| UTC cutover | The generic activation path now permits one future `00:00:00Z` first-of-month **non-OTA** version, keeps contiguous non-overlapping intervals, and serializes with invoice close. It marks the prior row `retired` when published, but interval selection continues to use it until the cutover. OTA publication remains blocked until the reviewed manifest and the remaining month/ownership policy are implemented. Preserve historical intervals and invoices. |
| Ownership/month policy | OTA-priced invoice close requires an exact UTC month and a current responsibility period that began no later than that month's start. Missing ownership evidence, a mid-month transfer, or Cloud closure leaves an explicit `incomplete` period for manual review. The current tenant preview now uses the UTC month once OTA is priced and withholds OTA estimates on partial/current-owner windows while retaining other service estimates; it reports `held_for_review` and disables the full-bill forecast. Still specify migration from historical profile-local months and any approved allocation or later-owner close policy without exposing another owner's data. |

The four approved prices do not answer these questions. Finance must also
review the real CDN cost and margin: the approved charge for a **verified
logical GiB** may be below a raw-egress research proxy in some regions.
CDN bytes, retries and Range requests are provider costs, not an extra
customer meter. Use the actual provider contract, regions, tiers, cache
behavior, retry/Range ratio, exchange rate and tax in the margin record.

## 3. Build and review the complete candidate card (partial P1 tooling)

The intended tool must consume the read-only consistent snapshot and the
approved manifest. Sort rate identities deterministically by
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
is current and no published future card exists, checks that existing rows are
unchanged and the four approved OTA meter identities are exact, then emits
the selected base and a deterministic candidate rate-set SHA-256. It does
**not** prove Finance approved the supplied tax category/rate, that the
account scope is implemented, or that the base will still be current when a
draft is later created. Store its output with restricted approval evidence;
it contains the full internal price card. A failed review leaves the database
unchanged.

Reject the candidate if any non-OTA row changes or disappears without its own
commercial approval, an OTA row is missing/duplicated/altered, another future
version conflicts, tax/scope is unresolved, the base has drifted, or an
issued/closing period intersects the proposed cutover. Create an immutable
draft only after a second reviewer accepts the exact manifest digest and the
future P1 transaction revalidates it. The current generic
`POST /v1/internal/billing/pricing-versions` accepts a caller-supplied rate
list and has none of these complete-card checks; **do not use it as this
procedure's preflight or publication mechanism**.

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

This section is the acceptance sequence for tooling that has **not** been
built. It is not a command to use today's immediate activation API.

1. Finance, Billing and operations recheck the approval packet and select the
   first **not-yet-started full UTC month** allowed by notice and contract
   terms. Record the precise `YYYY-MM-01T00:00:00Z` boundary. No past or
   in-progress month may be back-priced.
2. Re-run the read-only complete-card and account-scope diff against the
   target environment. Compare the current base, all pending versions and
   invoice/period states with the approved manifest digest. A mismatch aborts
   the publication before any write.
3. Use the future transactional publisher, once implemented and tested, to
   store the reviewed full version and its effective interval. Re-read the
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
| After scheduling but before the cutover | Use a **future, audited cancellation/replacement transaction only after it exists and is tested**; verify the old card remains selected. Today's API has no safe future OTA scheduling or cancellation. Never delete or edit a pricing row directly. |
| After the cutover, before invoice issue | Hold affected period close as incomplete and continue preserving receipts/outbox. Diagnose source, dual seals, CDN anomalies, scope/tax and rate selection. A corrective version may apply only from a later approved full UTC month. |
| After invoice issue | Preserve the immutable invoice and its rate/fact snapshots. Use the approved adjustment/credit and customer-notice process when implemented; do not rewrite the invoice, move old facts to another month, or backdate a new OTA rate. |

Pricing selection uses the version interval at the invoice **period start**.
Facts from a preactivation month remain audit evidence and are never later
included by rebuilding that month with a new card. A late report accepted in
the first priced month belongs to its server-acceptance UTC window under the
contract; a fact rejected by a closed-period barrier stays in the source
ledger for explicit reconciliation. An operational rollback cannot erase a
customer's usage evidence or make an already issued invoice mutable.
