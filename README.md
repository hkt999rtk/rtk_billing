# RTK Billing

`rtk_billing` is the monetary source of truth for RTK Cloud. It owns:

- versioned pricing and usage rating;
- commercial accounts and the append-only balance ledger;
- payment methods, consent, intents, provider adapters, and automatic top-up;
- billing profiles, periods, immutable invoices, PDF documents, and activity;
- organization billing access state.

Account Manager remains the source of truth for organizations, membership, and
RBAC. Cloud Admin authenticates the customer, checks Account Manager
capabilities, then calls this service with a dedicated service credential and
an explicit organization and actor context. Organization IDs are opaque UUIDs;
this service does not share Account Manager tables or database foreign keys.

## Run

```sh
export DATABASE_URL='postgres://rtk_billing:...@localhost:5432/rtk_billing'
export BILLING_SERVICE_TOKEN='at-least-32-characters'
export BILLING_INTERNAL_TOKEN='different-at-least-32-characters'
# Optional, enabled only as a pair and distinct from both tokens above:
export BILLING_DEBIT_TOKEN='different-debit-token-at-least-32-characters'
export BILLING_DEBIT_SOURCE='pricing-service'
go run ./cmd/server
```

`BILLING_SERVICE_TOKEN` is only for the tenant API used by Cloud Admin.
Pricing, usage, period-close, and access-control routes use
`BILLING_INTERNAL_TOKEN`; `/v1/internal/billing/debits` uses the dedicated
`BILLING_DEBIT_TOKEN`. The service refuses credential reuse across these
boundaries.

`BILLING_HANDOFF_TOKEN` optionally enables the dedicated Account Manager
coordinator routes below `/v1/internal/billing/clouds/{orgId}/ownership-handoffs/{operationId}`.
It must be at least 32 characters and distinct from every tenant, internal, debit
and provider credential. Leave it unset until the coordinated handoff deployment
gates pass; routes are absent by default. This credential is never issued to a
browser or Cloud Admin. See [handoff protocol](docs/cloud_ownership_handoff.md#internal-http-transport)
for scope, evidence and retry requirements.

`BILLING_CLOUD_CREATION_TOKEN` separately enables
`POST /v1/internal/billing/cloud-creations` for Account Manager's durable creation
outbox. It must be at least 32 characters and distinct from all other service
credentials. This initializes only a new account and its initial owner period;
existing accounts without a matching receipt require reviewed migration, never
automatic attribution to today's owner. No settlement or transfer is enabled by
this credential. Leave unset until the matching AM event worker is configured.

Health endpoints are unauthenticated. Tenant `/v1/orgs/...` operations require
`BILLING_SERVICE_TOKEN` plus trusted actor, permission, and request headers;
internal pricing/access and debit routes use their separate credentials.
Provider webhook and simulator callback routes authenticate their signed
payloads instead of accepting a bearer token.

The settlement collector is a separate process in the same image. It consumes
the authenticated Video Cloud usage horizon and independently reconciles local
usage, invoice and provider work before writing short-lived preflight or handoff
evidence:

```sh
export DATABASE_URL='postgres://rtk_billing:...@localhost:5432/rtk_billing'
export MQTT_USAGE_SETTLEMENT_BASE_URL='http://mqttusage.video-cloud.svc.cluster.local:8082'
export MQTT_USAGE_SETTLEMENT_TOKEN='dedicated-at-least-32-character-token'
go run ./cmd/settlement-collector
```

The token is collector-only and must not be reused for Billing tenant/internal,
Video Cloud ingest, Billing fact delivery or ownership-handoff authority. See
[PostgreSQL schema](docs/postgres-schema.md) and
[handoff protocol](docs/cloud_ownership_handoff.md) for the evidence boundary.

For local/staging NewebPay wire-contract testing, `cmd/payment-simulator`
optionally exposes synthetic MPG and Query endpoints when
`NEWEBPAY_MERCHANT_ID`, `NEWEBPAY_HASH_KEY`, `NEWEBPAY_HASH_IV`, and a
fixed `PAYMENT_SIMULATOR_NEWEBPAY_NOTIFY_URL`, plus a 32-character
`PAYMENT_SIMULATOR_ADMIN_TOKEN` are configured. It includes Cancel and Close
operations and a protected `/admin/newebpay` console. Billing may point the
real NewebPay adapter at that process with `NEWEBPAY_SIMULATOR_BASE_URL`.
The override is rejected for production, and the simulator never moves money.
When NewebPay is enabled, `NEWEBPAY_NOTIFY_URL` and `NEWEBPAY_RETURN_URL` are
fixed server configuration. The customer UI receives only an encrypted hosted
POST action; PAN, expiry, and CVV are entered solely on the provider page.

PayPal manual top-ups use the hosted Orders v2 checkout. Set `PAYPAL_ENABLED`,
`PAYPAL_ENVIRONMENT` (`sandbox` or `production`), `PAYPAL_CLIENT_ID`,
`PAYPAL_CLIENT_SECRET`, `PAYPAL_WEBHOOK_ID`, `PAYPAL_RETURN_URL`,
`PAYPAL_CANCEL_URL`, and `PAYPAL_AFTER_RETURN_URL` before enabling it. Configure
the PayPal app's `PAYMENT.CAPTURE.COMPLETED` webhook to call
`/v1/payment-webhooks/paypal`. The customer returns through the fixed Billing
return URL; Billing verifies the order and captured amount before crediting the
balance. The downloadable top-up PDF is a transaction detail, not a tax invoice.

For automatic transaction-detail email, run `cmd/payment-worker` with
`PAYMENT_EMAIL_ENABLED=true`, `SENDMAIL_HTTP_BASE_URL`,
`SENDMAIL_HTTP_BEARER_TOKEN`, and `PAYMENT_EMAIL_PORTAL_BASE_URL`. Billing queues
the email in the same database transaction as the balance credit and retries
delivery. It sends to the Billing profile's contact email when delivery is set
to `portal_and_email`. `PAYMENT_EMAIL_FROM` optionally selects a sender listed
in the SendMail service's `smtp.allowed_from`; an unset value uses that service's
default sender. Verify a new sender with the SendMail API before setting it.

## Test

```sh
go test ./...
TEST_DATABASE_URL='postgres://...' go test ./...
```
