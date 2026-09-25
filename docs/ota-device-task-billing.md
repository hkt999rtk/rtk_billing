# Product OTA Billing Design

Status: draft target design; four-meter pricing is proposed, not activated.

Owner: rtk_billing.

Last reviewed: 2026-09-26.

Canonical contract:
[Product OTA Delivery And Billing](../../rtk_cloud_contracts_doc/ota_delivery_and_billing.md).
This service document maps that contract to Billing's current fact, rate,
period, and invoice boundaries. It does not override the contract.

## Current Source And Gap

`internal/billing/types.go` already defines an immutable Product-scoped
`UsageFact` with `quantity_scale`. `internal/billing/invoice.go` groups facts
by Product and meter; `internal/billing/money.go` uses checked integer
arithmetic and rounds after aggregation. `internal/billingstore/pricing.go`
stores immutable facts and versioned rates. The preliminary
`internal/billing/ota.go` defines only `device_task` at a proposed rate.
The current close path in `internal/billingservice/service.go` does not
prove OTA source completeness. These are local source observations, not live
metering or invoice evidence.

The target has four `service_code=ota` meters:

| Metric | Unit | Quantity scale | Proposed TWD rate before tax |
| --- | --- | ---: | ---: |
| `device_task` | `tasks` | 0 | NT$96 / 1,000 |
| `successful_download_gib` | `GiB` | 9 | NT$0.96 / GiB |
| `artifact_storage_gib_month` | `GiB-month` | 9 | NT$0.96 / GiB-month |
| `artifact_write` | `requests` | 0 | NT$144 / million |

These are four separate customer items. The task is first durable assignment
per campaign/device, whether the dispatcher or a poll created it. The
successful-download item is the first accepted authenticated `downloaded`
report for a deployment and exact artifact SHA/size with a durable matching
artifact grant. After Product OTA disable, the producer may accept an existing
deployment's report only with its pre-disable matching grant and no later than
48 hours after that URL's exclusive expiry; the fact belongs to the server
acceptance month, even
if that is later than disable. URL grants, failed transfers and Range retries
do not charge. Object storage integrates actual
physical bytes over the UTC month until verified deletion, even for revoked or
disabled Products. Writes count successful object creations. OTA has no
additional customer object-read or raw CDN-egress fee.

## Fact Intake And Pricing

Video Cloud keeps immutable source receipts and enqueues one exact fact per
receipt in the shared durable Billing outbox. Billing authenticates the
producer, validates `organization_id`, server-resolved `product_id`, meter
code, unit, scale, UTC window and SHA-256, and rejects changed replay. Task
and download facts use a containing UTC-minute window at server commit time;
storage facts use the exact month. A late device report belongs to its
server-accepted month. Billing does not infer completed downloads from CDN
logs, object GETs, URL grants or installation success.

GiB facts use nano-units (`quantity_scale=9`) derived from actual 2^30-byte
GiB with checked half-up conversion. The storage producer retains exact
byte-second evidence and converts once per Product/month. The proposed
`PricingRate` values are `96/10^3`, `96/10^2`, `96/10^2`, and
`144/10^6` TWD respectively. Invoices aggregate by organization, Product,
service, metric and unit, then apply the rate version effective at period
start and round once per line. Tax policy belongs to the approved rate
version; this page does not set a tax rate.

Only an authorized commercial approval, full staging qualification and an
explicit pricing-version activation can start customer charges. The
effective date must be the next full UTC billing month or later. Historical
OTA receipts are not retroactively charged.

## Source-Complete Close

The target `POST /v1/internal/billing/ota-period-seals` accepts two
authenticated immutable seals for every organization/UTC month with OTA
pricing: `platform_grants` from Account Manager and `ota_producer` from
Video Cloud. The Platform seal enumerates every Product with any immutable
OTA-enabled grant revision before `period_end`, including disabled, retired
and zero-use Products. The producer seal includes Products with OTA facts or
stored OTA objects during the month. Billing requires both producer and fact
Product sets to be subsets of the Platform historical set; a producer-only
Product with no OTA grant history fails close. Billing also requires exact
reconciliation with immutable facts. Each seal has a stable global UUID,
unique organization/period/issuer scope, source high-water, Product set,
counts/digest, source digest and seal time. Same-content replay succeeds;
changed-content replay is a conflict.

The producer's `metric_counts` includes all four metric codes, with zero
where applicable. Its `fact_set_sha256` hashes Go JSON of sorted
`[usage_id, product_id, metric_code, quantity, quantity_scale, unit,
window_start, window_end, source_sha256]` rows. Timestamps are UTC
microseconds encoded RFC3339Nano; an empty set hashes JSON `[]`. The
Platform count map is empty and its fact digest absent. The contract defines
the complete serialization and high-water rules.

Closing with missing seals, outbox gaps, mismatched Product set, unknown
object ownership, or unreviewed CDN anomalies returns an auditable
`incomplete` result before invoice issuance. An empty fact set is not
proof of complete collection. A fact after a closed period requires the
approved adjustment/credit path; it cannot mutate an issued invoice.

## Verification Gate

Exercise fact replay and digest conflict, Product grouping, quantity scales,
TWD arithmetic, tax snapshots, zero-use Products, missing/changed seals,
source high-water gaps, storage over month boundaries, first-assignment
race, download report retries, and closed-period late facts. Run the
Account Manager to OTA to Billing to invoice chain against staging
PostgreSQL and the private-origin CDN before pricing activation. Record
rate approval, staging report, period-seal evidence, and effective date
alongside the pricing version.
