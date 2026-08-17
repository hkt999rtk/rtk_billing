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
go run ./cmd/server
```

Health endpoints are unauthenticated. All `/v1` and `/internal/v1` operations
require `Authorization: Bearer $BILLING_SERVICE_TOKEN`.

## Test

```sh
go test ./...
TEST_DATABASE_URL='postgres://...' go test ./...
```
