# PostgreSQL schema: multi-cloud ownership and settlement

This document describes the Billing-owned schema used by multi-Brand-Cloud
creation, unique-owner responsibility, deletion and ownership handoff. The SQL
migrations remain the executable source of truth; this page records the intended
relationships and invariants so a fresh database, a forward upgrade and the
service design can be reviewed together.

Account Manager owns `users`, Brand Clouds and the unique owner membership.
Billing does not copy membership rows or provide an owner-edit API. It stores an
evidence-backed responsibility projection against the opaque Brand Cloud UUID.

## Current migration boundary

A fresh database applies migrations through
`059_usage_period_barrier.sql`. Existing databases must apply every forward
migration in order. Resetting staging is useful for recovery rehearsal but is
not a substitute for forward migration verification and is never the production
upgrade strategy.

The settlement collector adds no new table: it reads Billing's existing usage,
period, invoice, ledger and provider-work tables, combines them with the
authenticated Video Cloud producer checkpoint, and writes the existing receipt
tables introduced by migrations 050 and 054.

## Cloud account and sole payer projection

```text
commercial_accounts (one per cloud and currency)
  1 ── * billing_responsibility_periods
  1 ── * billing_ownership_handoffs
  1 ── * billing_cloud_preflight_receipts
  1 ── 0..1 active billing_cloud_closures
```

- `commercial_accounts.organization_id` is the Brand Cloud UUID; the unique
  `(organization_id, currency)` key isolates balances between clouds even when
  one global user owns several clouds.
- `billing_responsibility_periods` records `owner_user_id`, positive
  `ownership_version`, effective bounds and source evidence. The partial unique
  index `billing_responsibility_current` permits exactly one open Billing payer
  period per account. It is a projection of Account Manager's sole owner, not a
  second ownership source.
- `billing_cloud_creation_receipts` binds a newly created cloud, initial owner,
  version 1, event ID and creation boundary. It cannot backfill or infer legacy
  responsibility.
- History is retained with `ON DELETE RESTRICT`; cloud deletion does not cascade
  into accounts, responsibility periods, invoices, ledger or audit evidence.

## Collector evidence

`billing_cloud_preflight_receipts` is the short-lived advisory evidence used by
deletion and ownership-eligibility checks. Each append-only row binds:

- account, current owner and ownership version;
- the complete local financial-state digest captured before collection;
- independently reconciled usage, invoice and provider checkpoint SHA-256s;
- financial evidence plus observation and expiry, with a database-enforced
  maximum five-minute lifetime.

`billing_handoff_settlement_receipts` provides the same three evidence domains
for a specific fenced handoff operation and operation version. Its linked
`billing_handoff_balance_snapshots` stores only a nonnegative TWD amount;
`billing_handoff_confirmations` records each participant's exact snapshot
confirmation. These tables are append-only and cannot themselves authorize the
Account Manager owner change.

An empty Billing fact table is not completeness proof. A usage checkpoint is
created only when the dedicated Video Cloud endpoint proves its Logger horizon
and Billing outbox acknowledgments through the exact cutoff, and Billing then
proves all corresponding accepted facts are rated and settled. Invoice and
provider checkpoints are created only when no open local period, invoice,
intent, job, attempt, setup, webhook or unallocated reversal remains.

## Handoff and closure state

- `billing_ownership_handoffs` allows one nonterminal operation per account and
  binds source/target, ownership version and cutoff. Its monetary fence is
  enforced by SQL triggers on payer authority and mutable financial state.
- Commit authorizations, committed decisions, finalizations, cancellations and
  abort acknowledgments are immutable protocol records. Finalization closes the
  old responsibility period and opens the new one without changing the balance.
- `billing_cloud_closures` and its revocation, settlement, completion,
  cancellation and release records implement non-cascading cloud closure. A
  closure and a handoff cannot be active together. Closed financial history and
  responsibility history are guarded against mutation.
- Transfer accepts a settled balance `>= 0`; deletion requires exactly zero.
  Both still require every independent usage, invoice, payment, refund, dispute
  and provider-work check.

## Usage integrity boundary

`billing_usage_facts` is immutable after acceptance (migration 058). Migration
059 serializes accepted usage against closing/closed invoice periods so a fact
cannot pass an earlier application precheck after waiting on a period close.
Corrections are new auditable facts, not row updates. The producer checkpoint
adds upstream delivery completeness; it does not weaken these local constraints.

## Reset and upgrade verification

For an explicitly authorized staging reset, back up the exact staging databases,
stop writers, recreate only the staging databases, apply all migrations, deploy
matching service images and run creation, settlement, deletion-preflight and
ownership-handoff acceptance. Production and retained audit databases must use
forward migrations and reviewed historical responsibility mapping, never the
staging reset shortcut.
