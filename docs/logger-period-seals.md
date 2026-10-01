# Logger source completeness and invoice close

Status: active implementation policy

Owner: rtk_billing

Last reviewed: 2026-10-02

The canonical [usage delivery and invoice contract](../../rtk_cloud_contracts_doc/pricing_and_invoicing.md#req-bill-usage-delivery-001-metered-usage-reaches-billing-through-an-immutable-transactional-outbox)
requires authenticated source completeness before issuance. Accepted facts,
an empty outbox, elapsed time, and financial settlement evidence alone cannot
prove that a Logger month is complete. This implementation covers Logger only;
it does not certify MQTT, Shadow, clip storage, or WebRTC source completeness.

## Source and delivery order

One authoritative shared PostgreSQL Logger ledger owns receipt sequencing,
coverage start, source-month freeze, and late-source rejection. Multiple Logger
replicas share that ledger; process-local counters or an empty replica queue
cannot certify a month. The producer retains an immutable environment source
identity and the timestamp when durable coverage began. A newly created ledger
cannot claim earlier months as zero usage. If coverage begins partway through a
month, that month remains held for review rather than being billed as complete.

After the full UTC month and a 24-hour finalization grace, the producer freezes
the covered ledger, verifies no overlapping pending receipt, produces both
`ingest_gib` and `retained_gib_month` per Product, and durably retains the source
seal with its outbox. A retained-only Product still emits `ingest_gib=0`.
Deliver all facts through the ordinary Billing internal fact API, receive exact
immutable acknowledgments, then submit the source seal through its separate
credential. Network ambiguity retains the same seal identity and payload for
retry; it never creates a replacement seal or discards source evidence.

## Dedicated seal API

`LOGGER_PRODUCER_SEAL_TOKEN` optionally enables
`POST /v1/internal/billing/logger-period-seals`. It must be at least 32
characters with no whitespace and must differ from tenant, internal fact,
OTA seal, grant-history, debit, handoff, cloud-creation, and provider secrets.
Only the Logger producer receives this credential. An unset token leaves the
route absent, while new Logger-priced invoices still require source evidence.

The strict JSON request contains:

| Field | Rule |
| --- | --- |
| `organization_id`, `seal_id` | Nonzero UUID; Product IDs are canonical lowercase UUIDs. |
| `period_start`, `period_end` | One complete half-open UTC calendar month. |
| `issuer_kind` | `logger_producer`. |
| `source_high_water` | Typed object: `schema_version=1`, nonzero `source_id`, `ledger=device_logger_receipts`, `coverage_from<=period_start`, `cutoff=period_end`, nonnegative `max_receipt_sequence` and `receipt_count`, `pending_count=0`. Receipt count cannot exceed the sequence horizon and includes at least one receipt per listed Product. |
| `product_ids` | Sorted unique Products covered by the frozen source; an empty set requires zero receipt and metric counts. |
| `metric_counts` | Exactly `ingest_gib` and `retained_gib_month`; each count equals the Product count. |
| `fact_set_sha256` | Digest of the exact fact set below. |
| `source_sha256` | Frozen source receipt digest. A zero-receipt source uses SHA-256 of `[]`. |
| `sealed_at` | At least 24 hours after period end, never in the future. |

Fact-set digest is SHA-256 of UTF-8 JSON containing an outer array, ordered by
`usage_id`. Each entry is exactly:

```text
[usage_id, product_id, metric_code, quantity, quantity_scale, unit,
 window_start_utc_rfc3339nano, window_end_utc_rfc3339nano, source_sha256]
```

Each Product has exactly one fact for each canonical metric, quantity scale 9,
the exact full-month window, `GiB` for ingestion and `GiB-month` for retention.
All overlapping Logger facts participate. Partial facts, extra Products,
unknown metrics, duplicate metrics, invalid units/windows, and mismatching
digests fail reconciliation. Zero sources use the digest of the empty fact array;
their authoritative coverage is still mandatory.

Responses are no-store and use
`{"logger_period_seal": <canonical seal>, "duplicate": <boolean>}`.
New reconciled evidence returns 201; exact replay returns 200. Changed evidence
returns 409. Missing or mismatching facts return retryable 503. Malformed JSON
or issuer returns 400; other credentials return 401. Payloads are bounded to
1 MiB. The producer verifies the entire echoed seal and duplicate/status binding.

## Database and close barriers

Forward migration `072_logger_period_seals.sql` adds immutable
`logger_period_seals`, uniqueness by organization/month and seal ID, plus an
overlap barrier for additional Logger facts after source acceptance. The API
locks the same TWD commercial-account row used by fact acceptance and invoice
close. Fact reconciliation and seal insertion share that transaction. The
database fact barrier runs after financial triggers acquire that account lock,
so a waiting concurrent insert rechecks the committed seal. Exact fact replay
returns its unchanged existing row; modified replay is rejected.

New invoices containing Logger prices or Logger facts require the complete
canonical two-meter price card, complete UTC month, current owner responsibility
for that entire month, matching recipient ownership version, and a reconciled
source seal. Missing evidence leaves `billing_periods.state=incomplete` with
`logger_pricing_incomplete`, `logger_period_not_utc_month`,
`logger_ownership_month_incomplete`, `logger_ownership_profile_mismatch`, or
`logger_source_incomplete`. A single fact from another service cannot bypass
this gate. Already issued invoice replay preserves the original invoice.

An authenticated zero-source month may issue a zero invoice when every priced
service has its own valid source seal. Logger evidence cannot certify another
service's zero use. Late sources are not moved to later months or used to
rewrite issued invoices; retain them for explicit reconciliation and correction.

Financial settlement and ownership-transfer checkpoints occur after invoice
issuance and retain their separate debt/payment/refund/dispute checks. They are
not substitutes for the pre-issue producer source seal.

## Usage preview and operation

Logger-priced previews select UTC months. Unsupported owner-partial or non-UTC
windows withhold Logger estimates, return `logger_estimate_status=held_for_review`
with a reason, and disable a complete-bill forecast. A legacy/partial Logger
card reports `pricing_incomplete`; no alias silently reprices canonical facts.
Other eligible service estimates remain visible. Current-month estimates remain
provisional; they do not prove month-end source completeness.

Deploy the canonical CI image's `/rtk-billing-migrate` Job first and verify
`072_logger_period_seals.sql` in `schema_migrations`, then roll out the API with
the dedicated credential. Keep `BILLING_DB_MIGRATE_ON_STARTUP=false` in protected
environments. The API runtime identity needs read/insert on the new table,
not update/delete authority. Reconcile the frozen producer receipt/outbox/seal
identities with Billing accepted facts and invoice lines before claiming
operational qualification. Unit/integration tests do not imply staging acceptance.
