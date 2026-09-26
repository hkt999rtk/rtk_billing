# Product OTA Billing Design

Status: four pre-tax unit prices approved; no OTA pricing version activated.

Owner: rtk_billing.

Last reviewed: 2026-09-26.

Canonical contract:
[Product OTA Delivery And Billing](../../rtk_cloud_contracts_doc/ota_delivery_and_billing.md).
This service document maps that contract to Billing's current fact, rate,
period, and invoice boundaries. It does not override the contract.

## Current Source And Gap

`internal/billing/types.go` defines an immutable Product-scoped
`UsageFact` with `quantity_scale`. `internal/billing/invoice.go` groups facts
by Product and meter; `internal/billing/money.go` uses checked integer
arithmetic and rounds after aggregation. `internal/billingstore/pricing.go`
stores immutable facts and versioned rates. `internal/billing/ota.go`
validates all four OTA meter contracts and defines approved but unactivated rates.
`internal/billingstore/ota_period_seals.go` accepts independent immutable
Platform and producer seals; `internal/billingstore/invoices.go` verifies
their Product sets, counts and fact digest before closing a priced OTA month.
These are tested local source capabilities, not live source collection,
staging CDN, invoice evidence or activated customer charging.

The four `service_code=ota` meters are:

| Metric | Unit | Quantity scale | Approved TWD rate before tax; not effective |
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
deployment's report only with its previously issued matching grant that passed
an enabled Product-grant check at issuance, and no later than 48 hours after
that URL's exclusive expiry. The fact belongs to the server acceptance month,
even if that is later than disable. URL grants, failed transfers and Range
retries do not charge. Object storage integrates actual physical bytes over
the UTC month until verified deletion, even for revoked or
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
byte-second evidence and converts once per Product/month. The approved
`PricingRate` values are `96/10^3`, `96/10^2`, `96/10^2`, and
`144/10^6` TWD respectively. Invoices aggregate by organization, Product,
service, metric and unit, then apply the rate version effective at period
start and round once per line. Tax policy belongs to the approved rate
version; this page does not set a tax rate.

Only an authorized commercial approval, full staging qualification and an
explicit pricing-version activation can start customer charges. The
effective date must be the next full UTC billing month or later. Historical
OTA receipts are not retroactively charged.

While the selected pricing version contains **zero** OTA rates, Billing
retains accepted OTA facts as immutable evidence but excludes them from
invoice lines, usage estimates, and billable fact counts. Those facts alone
do not satisfy the non-OTA usage requirement for closing a mixed-service
month. A non-OTA fact without a rate, or a partial/invalid OTA rate set,
still fails closed. When a future version contains all four OTA rates,
its effective interval selects only that month's facts; prior months keep
their old version. The current immediate activation API deliberately
rejects every OTA-containing version until future UTC-month scheduling and
commercial qualification are complete. A draft version or this code change
cannot start customer charges. The tenant usage API's `fact_count` and
`usage_through` describe only the facts used for priced lines and forecasts;
the immutable Billing fact table and producer receipt/outbox remain the
operational audit sources for excluded OTA facts.

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
proof of complete collection. Verified empty seals permit a zero-use close
only when the active pricing version contains OTA rates exclusively. A mixed
pricing version retains the nonempty usage-fact requirement because OTA
seals do not attest its other services. A fact after a closed period requires
the approved adjustment/credit path; it cannot mutate an issued invoice.

## Verification Gate

Exercise fact replay and digest conflict, Product grouping, quantity scales,
TWD arithmetic, tax snapshots, zero-use Products, missing/changed seals,
source high-water gaps, storage over month boundaries, first-assignment
race, download report retries, and closed-period late facts. Run the
Account Manager to OTA to Billing to invoice chain against staging
PostgreSQL and the private-origin CDN before pricing activation. Record
rate approval, staging report, period-seal evidence, and effective date
alongside the pricing version.
