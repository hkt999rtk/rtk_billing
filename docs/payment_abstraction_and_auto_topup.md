# Payment Abstraction And Automatic Top-Up Design

The design-first multi-cloud extension is specified in
[cloud_ownership_handoff.md](cloud_ownership_handoff.md): unique owner tenant
Billing authority, per-cloud balance, responsibility history and fenced handoff.
It is not included in the implemented-foundation claim below.

Status: implemented foundation with guarded rollout. Provider-neutral money,
ledger, policy, intent, durable worker/reconciliation, safe customer HTTP APIs,
and verified webhook ingestion are implemented. Durable hosted setup, charge,
query, refund, reconciliation, and failure behavior are qualified with both the
deterministic in-process fake provider and the non-production payment simulator
defined by workspace `docs/payment-simulator.md`; NewebPay hosted setup and merchant-initiated
charging remain disabled until written capability approval and sandbox
qualification are available.

Owner: `rtk_billing`.

Contract source of truth:

- `repos/rtk_cloud_contracts_doc/billing_service.md`
- `repos/rtk_cloud_contracts_doc/payments_and_balance.md`
- `repos/rtk_cloud_contracts_doc/pricing_and_invoicing.md`
- `repos/rtk_cloud_contracts_doc/billing_activity.md`

## Decision Summary

The independent Billing service owns commercial settlement in its own process,
repository, and PostgreSQL schema. Account Manager owns organization identity,
membership, and RBAC only. Cloud Admin resolves that authorization and calls
Billing through a dedicated tenant credential and explicit actor, organization,
permission, and request context.

The design provides:

- one commercial account per Brand Cloud and currency;
- an append-only PostgreSQL monetary ledger;
- provider-neutral payment methods, intents, attempts, and webhook inbox;
- a customer-consented automatic top-up policy;
- a provider adapter registry whose first qualification adapters are the
  deterministic fake and hosted simulator, followed by guarded NewebPay;
- durable reconciliation and an emergency provider-disable control.
- versioned pricing, immutable usage facts, period close, invoice snapshots,
  digest-anchored PDF documents, billing activity, and access state.

It does not make Billing the source of raw service telemetry. Video Cloud and
other services continue to produce usage facts; trusted producers submit those
facts through the Billing internal API. Billing applies versioned prices,
closes a period, issues an immutable invoice, and posts the corresponding debit
without coupling usage producers to a payment provider.

## Implemented Baseline And Remaining Gap

The repository now contains commercial accounts, an immutable monetary ledger,
payment consent and safe method metadata, automatic top-up policy state,
payment intents/attempts, a webhook inbox, durable reconciliation jobs,
pricing, invoice/document/activity stores, HTTP APIs, three isolated service
credential boundaries, a non-production simulator, and a dedicated payment
worker. Account Manager organization `tier` fields do not represent money or
payment status.

Remaining production dependencies are the NewebPay mapping for approved
hosted payment-method setup, written approval for unattended merchant-initiated
charging, sandbox credentials and qualification evidence, and finance/legal/
security approval. Until those dependencies are met, the NewebPay adapter
reports unsupported capabilities and production charging stays disabled.

The workspace already has durable patterns that can be reused:

- PostgreSQL migrations and store transactions;
- outbox/inbox idempotency and worker retry conventions;
- actor-aware audit events;
- permission-based organization scope;
- redacted structured logs and request correlation.

The payment design must reuse these operational patterns without coupling
payment correctness to the provisioning outbox or Redis.

## Architecture Boundary

```text
Cloud Admin UI/BFF
       |
       | Account Manager identity/RBAC context
       v
RTK Billing tenant HTTP API
       |
       v
payment application service
  |        |             |
  v        v             v
ledger   policy      payment orchestrator
  |        |             |
  +--------+-------------+
           |
 isolated Billing PostgreSQL
           |
           v
 payment worker/reconciler ---> PaymentProvider interface ---> simulator / NewebPay
           ^                                                    |
           +---------------- verified webhook inbox <-----------+

usage producers ---> Billing usage/pricing/period-close API ---> invoice + debit
external debit producer -----------------> POST /v1/internal/billing/debits
                                             (dedicated debit credential)
```

HTTP handlers validate transport and authorization. They do not implement
provider cryptography or ledger arithmetic. The application service owns
transactions and domain transitions. Adapters own provider request/response
translation only.

## Package Layout

```text
internal/payment/
  types.go            commercial account, ledger, policy and provider-neutral types
  money.go            integer minor-unit arithmetic and reason validation
  intent.go           provider-neutral state machine
  policy.go           threshold, generation, limits, and re-arm rules
  orchestration.go    provider result and attempt rules
  provider.go         PaymentProvider interface and capability vocabulary
  errors.go           stable domain errors

internal/paymentstore/
  store.go            PostgreSQL scans and shared validation
  accounts.go
  ledger.go
  methods.go
  intents.go
  policy.go
  customer.go
  webhooks.go
  orchestration.go

internal/paymentprovider/newebpay/
  adapter.go
  crypto.go
  request.go
  response.go
  webhook.go
  errors.go

internal/api/
  payments.go

cmd/payment-worker/
  main.go
```

Files may be split further as the implementation grows. The mandatory boundary
is enforced: `internal/payment` does not import Gin, PostgreSQL drivers, or a
provider implementation.

## Provider Interface

The application layer consumes a narrow interface:

```go
type PaymentProvider interface {
    Name() string
    Capabilities(context.Context) Capabilities
    CreateSetup(context.Context, SetupRequest) (SetupResult, error)
    Charge(context.Context, ChargeRequest) (ChargeResult, error)
    Query(context.Context, QueryRequest) (QueryResult, error)
    VerifyWebhook(context.Context, WebhookRequest) (WebhookEvent, error)
    Refund(context.Context, RefundRequest) (RefundResult, error)
}
```

Implementation rules:

- `ChargeRequest` contains a provider-neutral amount, currency, opaque method
  reference, merchant order reference, and stable idempotency/correlation key;
- the interface never carries PAN or CVV;
- unsupported operations return a typed capability error, not a false success;
- adapter errors map to `declined`, `temporary`, `invalid_request`,
  `authentication`, `requires_action`, or `unknown` while preserving a
  redacted provider code for support;
- `unknown` is mandatory when a request may have reached the provider;
- orchestration persists intent and attempt state before and after the call;
- no adapter writes Billing tables directly.

`Refund` may initially return unsupported. It is included because refund and
chargeback behavior changes the ledger and must not later bypass the
abstraction.

Refund and chargeback ledger compensation is fail-safe even while the provider
operation remains unsupported: a compensating debit is idempotent, disarms the
automatic top-up policy, and moves an otherwise-active commercial account to
`attention_required`. It never starts a replacement charge.

## Data Model

All monetary fields are `BIGINT`, are interpreted as ISO currency minor units,
and are never floating point. TWD has zero fractional digits, so one internal
TWD `amount_minor` unit is NT$1. UI and provider adapters do not multiply or
divide TWD by 100. Stored timestamps are UTC. Provider references are treated as opaque,
length-bounded strings.

The ownership, collector-evidence and cloud-closure tables added by migrations
049-059 are documented in [PostgreSQL schema: multi-cloud ownership and
settlement](postgres-schema.md). The SQL migrations remain executable truth;
staging reset does not replace a forward upgrade path.

### `commercial_accounts`

| Column | Notes |
| --- | --- |
| `id UUID PK` | Internal account identity. |
| `organization_id UUID NOT NULL` | Owning Brand Cloud; unique with currency. |
| `currency CHAR(3) NOT NULL` | Initially `TWD`. |
| `available_balance_minor BIGINT NOT NULL` | Transactional projection of ledger sum. |
| `state TEXT NOT NULL` | `active`, `attention_required`, `suspended`, `closed`. |
| `version BIGINT NOT NULL` | Monotonic projection version. |
| `created_at`, `updated_at` | UTC audit times. |

Unique: `(organization_id, currency)`.

### `balance_ledger_entries`

| Column | Notes |
| --- | --- |
| `id UUID PK` | Immutable entry ID. |
| `account_id UUID NOT NULL` | Commercial account. |
| `direction TEXT NOT NULL` | `credit` or `debit`. |
| `amount_minor BIGINT NOT NULL CHECK > 0` | Positive magnitude. |
| `currency CHAR(3) NOT NULL` | Must match account. |
| `reason TEXT NOT NULL` | Contract reason code. |
| `idempotency_scope`, `idempotency_key` | Unique per account. |
| `external_type`, `external_id` | Invoice, payment intent, adjustment, refund, or chargeback reference. |
| `balance_after_minor BIGINT NOT NULL` | Auditable projection after this entry. |
| `actor_type`, `actor_id`, `request_id` | Attribution. |
| `created_at` | Immutable commit time. |

No update/delete repository methods are provided. Corrections append a
compensating entry.

### `payment_consents`

Stores consent type/version, rendered-text SHA-256, accepted actor, accepted
time, locale, client surface, and revocation time/reason. It stores no card
data. Existing evidence remains after revocation.

### `payment_methods`

Stores account ID, provider, opaque customer/method references, safe display
metadata, provider capability snapshot, lifecycle state, consent ID, and
timestamps. An account may have several methods but only one policy-selected
default. Revocation is a state transition, not deletion.

### `payment_method_setup_sessions`

Stores the account/provider-scoped idempotency key, canonical request SHA-256,
correlation ID, pending method ID, normalized setup state, safe provider code,
and SHA-256 of the short-lived hosted URL. Consent and a pending method commit
before provider I/O. The hosted URL, session token, opaque provider references,
PAN, and CVV are never stored in this table. Completed opaque references are
encrypted before the linked method becomes active.

### `auto_topup_policies`

Stores account ID, enabled state, threshold, top-up amount, currency,
payment-method ID, daily attempt/amount limits, cooldown, generation, armed
state, last trigger/success time, consent ID, actor, and timestamps.

`generation` increments on every policy replacement. The open-intent uniqueness
rule includes account and generation.

### `payment_intents` And `payment_attempts`

An intent stores account, amount, currency, reason, policy generation, provider,
method ID, normalized state, internal idempotency key, merchant order reference,
provider transaction reference, correlation ID, and timestamps.

Attempts are append-only observations of provider calls. They store operation,
attempt number, start/completion time, normalized result, redacted provider
code, request/response evidence digest, and next reconciliation time. Secrets
and provider payloads are never stored in these columns.

Recommended unique constraints:

```text
(account_id, idempotency_key)
(provider, merchant_order_reference)
(provider, provider_transaction_reference) WHERE reference IS NOT NULL
(account_id, policy_generation) WHERE automatic intent is open
(intent_id, attempt_number)
```

The partial uniqueness expression will be implemented using a generated/open
marker or an equivalent PostgreSQL partial index after migration review.

### `payment_webhook_inbox`

Stores provider, provider event reference when present, SHA-256 of the received
body, verification result, mapped intent, normalized event type, processing
state, received/processed time, and a redacted summary. Unique provider event
reference or payload digest provides replay safety.

### `payment_reconciliation_jobs`

Stores intent ID, reason, status, due time, attempt count, lease time, and safe
last error. PostgreSQL is the durable queue. Redis may wake workers but cannot
be the only record.

## Transaction Boundaries

### Debit And Threshold Evaluation

1. Begin PostgreSQL transaction.
2. Resolve the account from trusted organization identity.
3. Lock `commercial_accounts` with `FOR UPDATE`.
4. Insert the debit using its unique idempotency tuple.
5. Update the projected balance and version.
6. Evaluate the active policy against the new committed projection.
7. If eligible, create exactly one `created` automatic intent and a worker job.
8. Commit.

Provider I/O never occurs inside this database transaction.

### Provider Charge

1. Lease a `created` intent in PostgreSQL.
2. Persist a `processing` attempt before network I/O.
3. Call the adapter with the stable merchant order/idempotency reference.
4. Persist the normalized result.
5. An authorization-only response transitions to `authorized` and does not
   credit. On confirmation of the finance-approved capture/completion point,
   lock account and intent, transition once to `succeeded`, and append one
   credit in the same transaction.
6. On timeout or ambiguous response, transition to `unknown` and schedule
   query reconciliation using the same provider transaction/order reference.

### Webhook

1. Read a bounded body and compute its digest.
2. Ask the adapter to verify and normalize it.
3. Insert/dedupe the inbox record.
4. Validate merchant, amount, currency, and internal/provider references.
5. Schedule reconciliation or apply a conclusive legal transition.
6. On success, append the credit through the same service method used by query
   reconciliation.

Duplicate callbacks return a successful acknowledgement after dedupe when the
original event was valid. Invalid callbacks never reveal which internal intent
exists.

## Automatic Top-Up State

The policy is crossing-based, not a loop that polls and charges while the
balance remains low.

```text
armed --balance < threshold--> intent open --> succeeded --> disarmed
  ^                                  |              |
  |                                  |              +--> attention_required
  |                                  |                   if still below threshold
  |                                  +--> failed/unknown --> cooldown/reconcile
  |
  +-- balance >= threshold or authorized policy generation update
```

Daily limits use `Asia/Taipei` calendar days and reset at local midnight. The
API returns the timezone and reset instant explicitly.

Approved simulator guardrails:

| Setting | Proposed default | Constraint |
| --- | ---: | --- |
| `threshold_minor` | 300 | positive TWD integer; trigger only below threshold |
| `top_up_amount_minor` | 300 | positive TWD integer |
| `daily_attempt_limit` | 2 | configurable, 1-10 |
| `cooldown_seconds` | 3600 | at least 300 |
| `daily_amount_limit_minor` | 1000 | configurable; finite and at least one top-up |
| conclusive failure limit | 3 | disable policy and require owner re-enable |
| top-up recursion | disabled | one success per crossing/generation |

Successful automatic charge resets the consecutive-failure counter. Terminal
failed/canceled charge increments it. `unknown` does not increment until
reconciliation is conclusive. `requires_action` immediately pauses for owner
attention but is not counted as a conclusive failure.

The service rejects a policy that has no finite daily amount limit. Environment
configuration may impose a stricter platform maximum than the customer policy.

## API Behavior

The contract document defines the route families. Exact request/response
schemas, security schemes, and errors are declared in `rtk_billing/openapi.yaml`
and exercised by integration route-conformance tests.

Common rules:

- Cloud Admin resolves organization membership and permission, then Billing
  requires one exact trusted permission plus its own access-state gate;
- create/update requests require `Idempotency-Key` where a side effect may
  occur;
- money is encoded as an integer `amount_minor` plus ISO currency;
- list APIs use bounded cursor pagination;
- write responses return normalized internal state, never provider secrets;
- policy updates require the consent version and accepted confirmation;
- setup responses may return a short-lived hosted URL but logs and audit must
  redact its query and token components;
- optimistic update uses policy `version`/ETag to prevent stale changes.

`POST /v1/internal/billing/debits` is disabled unless both
`BILLING_DEBIT_TOKEN` and `BILLING_DEBIT_SOURCE` are configured. The debit
token must differ from `BILLING_SERVICE_TOKEN` and `BILLING_INTERNAL_TOKEN`.
It accepts only
`invoice_debit` and `usage_adjustment_debit`, requires an immutable external
reference and `Idempotency-Key`, and records the configured source as the
ledger actor. It cannot create credits, refunds, chargebacks, manual
adjustments, prices, or invoices.

Debit ingestion does not create accounts. If the immutable new-cloud creation
event has not provisioned Billing yet, it returns
`503 BILLING_ACCOUNT_NOT_READY` without ledger or account writes. Retry the
same idempotency key and payload after provisioning; do not initialize an
owner from a debit request. Internal period closing has the same prerequisite.

Stable error codes:

```text
PAYMENT_PROVIDER_UNAVAILABLE
PAYMENT_PROVIDER_NOT_CONFIGURED
PAYMENT_CAPABILITY_UNSUPPORTED
PAYMENT_METHOD_REQUIRED
PAYMENT_METHOD_INACTIVE
PAYMENT_CONSENT_REQUIRED
PAYMENT_AMOUNT_INVALID
PAYMENT_CURRENCY_UNSUPPORTED
PAYMENT_INTENT_CONFLICT
PAYMENT_STATUS_UNKNOWN
PAYMENT_METHOD_SETUP_CONFLICT
PAYMENT_REFERENCE_PROTECTION_UNCONFIGURED
PAYMENT_REFERENCE_PROTECTION_FAILED
PAYMENT_PROVIDER_RESPONSE_INVALID
AUTO_TOPUP_LIMIT_REACHED
AUTO_TOPUP_POLICY_CONFLICT
BILLING_ACCOUNT_SUSPENDED
BILLING_DEBIT_UNCONFIGURED
BILLING_DEBIT_REASON_INVALID
BILLING_DEBIT_CONFLICT
```

Decline details are customer-safe and do not expose fraud/risk signals.

## Authorization And Audit

The contract reserves the billing/payment permission actions. Initial role
mapping proposal:

| Actor | Read account/ledger/intents | Manage method/policy | Manual top-up | Reconcile |
| --- | --- | --- | --- | --- |
| Brand Cloud owner | Yes | Yes | Yes | No |
| Brand Cloud admin | Configurable explicit grant | Configurable explicit grant | Configurable explicit grant | No |
| Member/read-only | Optional read grant | No | No | No |
| Platform support | Redacted read with audited scope | No | No | No |
| Payment worker service | Required internal subset | No customer policy writes | Execute existing intent only | Yes |

Every customer payment mutation writes the Billing audit envelope with payment
resource type and request correlation. Support tooling must not display provider
credentials, full provider payloads, hosted-session tokens, card data, or
unredacted customer identifiers.

## Payment Simulator

The approved first hosted provider is available only in local, CI, and shared
staging. Its public staging origin is
`https://payment-simulator.video-cloud-staging.realtekconnect.com`. It uses a
dedicated process, durable synthetic state, signed setup callbacks, and the
same provider, intent, reconciliation, encrypted-reference, and ledger
interfaces as a real provider. Startup fails if enabled in production. The
page contains no card-entry fields and always displays
`TEST PAYMENT - NO REAL CHARGE`.

The normative protocol, configuration, scenarios, Test IDs, and cleanup rules
are in workspace `docs/payment-simulator.md`.

## NewebPay Adapter Design

The adapter name is `newebpay`. Domain and API resources do not use NewebPay
field names.

### Credentials And Configuration

Expected secret references:

```text
NEWEBPAY_MERCHANT_ID
NEWEBPAY_HASH_KEY
NEWEBPAY_HASH_IV
PAYMENT_REFERENCE_ENCRYPTION_KEY
```

`PAYMENT_REFERENCE_ENCRYPTION_KEY` must be a base64-encoded 32-byte key. It
encrypts opaque provider customer and method references at rest and must be
managed and rotated as a deployment secret.

Expected non-secret configuration:

```text
NEWEBPAY_ENVIRONMENT=sandbox|production
NEWEBPAY_ENABLED=false
NEWEBPAY_MERCHANT_INITIATED_CHARGE_ENABLED=false
PAYMENT_WORKER_ENABLED=false
PAYMENT_WORKER_POLL_INTERVAL
PAYMENT_WORKER_BATCH_SIZE
PAYMENT_WORKER_LEASE_DURATION
PAYMENT_RECONCILIATION_DELAY
```

Secret values come from the deployment secret manager and are validated at
startup without logging. A missing or malformed provider configuration disables
the adapter and fails payment writes with a typed error; it must not prevent
identity/registry APIs from starting.

### Provider-Specific Constraints

- `MerchantOrderNo` is length/character constrained; map the internal UUID to a
  unique prefixed reference that fits the provider limit and persist the map.
- NewebPay `Amt` is an integer New Taiwan dollar value and maps directly from
  internal TWD `amount_minor`; there is no factor-of-100 conversion.
- `TokenTerm` is not a public customer ID. Derive a stable non-identifying,
  length-bounded opaque value and persist the association.
- AES and integrity/check computation are isolated in a small tested crypto
  module with official fixture coverage.
- accepted callback data is decrypted/verified before field use;
- redirect/return URLs are browser UX signals; server-side NotifyURL or query
  reconciliation determines durable payment state;
- callback/query responses are normalized before the application sees them.

The public NewebPay documents demonstrate remembered-card and fixed periodic
payment flows, but do not by themselves prove that this merchant may perform
variable-time threshold top-ups. The provider must confirm and enable the
required capability. Until then the adapter reports
`merchant_initiated_charge=false` and policy enablement fails closed.

### Operational Prerequisites

- enterprise merchant account and enabled payment products;
- sandbox and production credentials stored separately;
- registered HTTPS callback URLs;
- stable egress IPs approved by NewebPay where required;
- dedicated sandbox payer/test cards;
- merchant consent and modification/cancellation records;
- finance-owned refund, chargeback, and reconciliation procedures.

## Security And Compliance Controls

- Prefer provider-hosted or embedded-tokenized setup. Never proxy raw card
  fields through Billing, Account Manager, or Cloud Admin.
- Never store CVV/CVC under any condition.
- Encrypt opaque provider method/customer references at rest when they permit
  charging; keys live outside PostgreSQL.
- Restrict provider credentials to the payment worker and webhook verifier.
- Separate test and production merchant IDs, keys, callbacks, and data.
- Add SSRF-safe fixed provider base URLs; do not accept them from API requests.
- Bound callback body size, validate content type, and apply rate limiting.
- Redact common secrets plus provider field names before logs/artifacts.
- Preserve consent, policy, intent, ledger, refund, and chargeback audit evidence
  according to finance/legal retention decisions.
- Run dependency, SAST, secret, and redaction checks on adapter changes.
- Use a runtime provider kill switch that blocks new charges but still permits
  status query, reconciliation, and read APIs.

This design reduces PCI scope by avoiding card-data handling but does not claim
PCI exemption. Security/compliance review must classify the final hosted or
embedded flow before launch.

## Observability And Support

Metrics use low-cardinality provider/environment/status labels and never account
or transaction IDs as labels. Initial metrics:

```text
rtk_billing_payment_intents_total
rtk_billing_payment_attempts_total
rtk_billing_payment_reconciliation_backlog
rtk_billing_payment_unknown_age_seconds
rtk_billing_payment_webhooks_total
rtk_billing_auto_topup_triggers_total
rtk_billing_balance_reconciliation_mismatches_total
```

Structured logs include request/correlation ID, internal intent ID, provider,
normalized state, redacted provider code, operation, duration, and outcome.
Provider transaction references appear only in restricted fields and are
masked in general logs.

Alerts:

- any ledger/projection mismatch;
- oldest unknown intent above the reconciliation SLO;
- webhook authentication failures above baseline;
- callback-to-query disagreement;
- repeated automatic top-up failures;
- provider authentication/configuration failure;
- reconciliation backlog or worker lease stalls.

## Test Plan

### Unit

- integer credit/debit arithmetic and overflow boundaries;
- invalid currency, zero/negative amount, and reason validation;
- ledger idempotency and compensation;
- all legal/illegal intent transitions;
- strict `< threshold` behavior, equality behavior, re-arm, generation, cooldown,
  daily limits, inactive method, and no recursive charging;
- refund/chargeback compensation disarms automatic top-up and cannot initiate a
  replacement charge;
- provider error normalization and timeout-to-unknown;
- NewebPay order/token-term mapping, crypto, integrity, malformed data, and
  redaction fixtures.

### PostgreSQL Integration

- concurrent debits produce a correct projection and one top-up intent;
- duplicate debit, callback, query, and worker execution converge;
- success and credit commit atomically;
- process failure at each transition resumes safely;
- row locks do not cross tenant boundaries;
- account/ledger reconciliation detects injected drift;
- revoked method/policy races cannot initiate a new charge.

### Provider Contract And E2E

- `rtk-cloud test-payment --profile fake-e2e` maps the three active hosted
  setup and automatic top-up E2E Test IDs to canonical Go operations and produces a case-level
  JSON, Markdown, JUnit, timing, SHA-256, redaction, and cleanup report;
- setup uses the provider-hosted/tokenized surface and stores no card data;
- sandbox success, decline, timeout, duplicate/out-of-order callback, query,
  cancel/refund where supported, and credential failure;
- one debit crossing creates one provider transaction and one credit;
- callback amount, merchant, or signature mismatch cannot credit;
- provider-disabled mode makes no new external request;
- artifacts pass secret/card-data redaction scans.

### UI And Live Staging

- desktop/mobile owner setup, validation, enable, update, disable, method revoke,
  limit reached, requires-action, failure, unknown, and success status;
- final screenshot per UI case/target without card or secret material;
- dedicated sandbox merchant and run-scoped Brand Cloud/test data only;
- database ledger, intent, provider query, callback, and runtime logs correlate
  to one run ID;
- cleanup revokes test methods and deletes run-scoped customer test data while
  retaining the redacted test report.

## Delivery Sequence And Definition Of Done

1. Accept contracts and this architecture document.
2. Confirm provider capability, merchant onboarding, consent, and finance rules.
3. Add exact OpenAPI schemas and planned Test IDs/catalog entries.
4. Add schema and pure domain/store implementation with unit/integration tests.
5. Add a fake provider and complete orchestration tests.
6. Add and qualify the hosted non-production payment simulator.
7. Add the NewebPay adapter behind disabled feature flags.
8. Add Cloud Admin BFF/UI and desktop/mobile evidence.
9. Pass sandbox live staging, reconciliation, refund, duplicate callback, and
   cleanup tests.
10. Enable one allowlisted canary Brand Cloud with conservative limits.
11. Expand only after ledger reconciliation and support metrics remain clean.

Implementation is not complete until the exact API is in `openapi.yaml`, all
reserved critical Test IDs have executable sources, required reports are PASS,
and automatic charging remains disabled by default outside the allowlist.
