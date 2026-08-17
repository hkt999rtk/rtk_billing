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

Health endpoints are unauthenticated. Tenant `/v1/orgs/...` operations require
`BILLING_SERVICE_TOKEN` plus trusted actor, permission, and request headers;
internal pricing/access and debit routes use their separate credentials.
Provider webhook and simulator callback routes authenticate their signed
payloads instead of accepting a bearer token.

## Test

```sh
go test ./...
TEST_DATABASE_URL='postgres://...' go test ./...
```
