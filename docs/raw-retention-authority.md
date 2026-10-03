# Raw Billing Retention Authority

Status: implemented service behavior; disabled by default; no environment is
qualified or enabled by this document.

Classification: source.

Owner: `rtk_billing` (financial approval and durable decision authority).

Last reviewed: 2026-10-03.

## Boundary and Deployment Gate

Billing authorizes removal of Logger's hot payloads. It never deletes Billing
facts, invoices, Video Cloud PostgreSQL receipts, cloud archives or decryption
keys. Follow the workspace raw-data lifecycle and key custody designs and the
canonical billing contracts for cross-service requirements. Daily backups and
verification can run while retirement remains disabled.

Apply migrations `070_raw_retention_authority.sql` and
`071_raw_retention_unaccepted_cancellation.sql` with the migration-owner
identity before enabling the API. For protected environments keep
`BILLING_DB_MIGRATE_ON_STARTUP=false`. Enable Dev, then qualify Staging before
any approved Prod activation. Migration does not approve a policy or install a
scheduled cleanup job.

`BILLING_RAW_RETENTION_ENABLED=false` is the default. Unconfigured routes are
absent. Retention routes are always absent (404) from the public Billing router,
even for valid authority credentials. Enabling starts a separate private HTTP
listener; expose it only through an internal ClusterIP and restrictive network
policy, never through the public ingress. Both listeners shut down gracefully;
failure to bind the private port prevents startup of either listener.
Enabling requires all of the following:

| Configuration | Purpose |
| --- | --- |
| `BILLING_RAW_RETENTION_ENVIRONMENT` | Actual logical environment scope; defaults to existing `ENVIRONMENT`. Renderer pins the selected environment without changing legacy producer/collector identity. |
| `BILLING_RAW_RETENTION_LISTEN_ADDR` | Dedicated private listener, default `:8081`; must differ from the public `PORT`. |
| `BILLING_RAW_RETENTION_FINANCIAL_TOKEN` | Policy creation/activation, explicit period clearance and holds. |
| `BILLING_RAW_RETENTION_RECOVERY_TOKEN` | Independent recovery-owner policy approval. |
| `BILLING_RAW_RETENTION_CONTROLLER_TOKEN` | Automatic operation creation, abort request and outcome reconciliation. |
| `BILLING_RAW_RETENTION_AUTHORITY_READ_TOKEN` | Logger-only `GET` of a decision; cannot create or resolve one. |
| `BILLING_RAW_RETENTION_LOGGER_BASE_URL` / `LOGGER_TOKEN` | Billing directly reads exact durable Logger terminal status using Logger's separate read-only `RTK_CLOUD_LOGGER_BILLING_LIFECYCLE_READ_TOKEN`; no capture/apply/abort permission. |
| `BILLING_RAW_RETENTION_CONSUMER_ID` / `CONSUMER_BASE_URL` / `CONSUMER_TOKEN` | Registered consumer origin and its separate reconciliation credential. Register the existing `video-cloud-mqttusage/<collector environment>` ID, which may differ from the logical retention environment. |
| `BILLING_RAW_RETENTION_VERIFIER_KEY_ID` / `VERIFIER_PUBLIC_KEY` | Approved Ed25519 verifier ID and base64-encoded 32-byte public key. No private signing/decryption key goes to Billing. |

All suffixes in the last four rows retain the `BILLING_RAW_RETENTION_` prefix.
The six bearer credentials must be distinct 32+ character values and may not
reuse tenant, pricing/usage, handoff, payment or other service credentials.
Origins are fixed HTTPS, or loopback/Kubernetes service HTTP; userinfo, paths,
query parameters, fragments and redirects are refused. Logical environments
are `dev`, `staging` and `prod` (`development`/`production` runtime aliases map
to `dev`/`prod`). Do not reset existing collector identity to change retention
scope; register its current consumer ID even if a legacy deployment used a
different service environment label.

## Approval and Automatic Eligibility

Base route: `/v1/internal/billing/raw-retention`. All responses are `no-store`.
Bodies are bounded strict JSON; receipt sequences are decimal JSON strings.

1. Financial authority `POST /policies` records an immutable policy version:
   `policy_id`, integer `version`, `environment`, `store_id`,
   `hot_retention_days` (at least 90), exact `required_consumers` and
   `financial_approval_ref`. Unknown consumers fail closed. The returned
   `policy_sha256` binds the complete specification.
2. Independent recovery authority `POST /policies/{policyId}/{version}/approve`
   with `policy_sha256` and `recovery_approval_ref`. Financial authority then
   `POST .../activate` with `policy_sha256`. `POST .../deactivate` prevents
   future operations but cannot revoke an already ACTIVE decision.
3. Financial authority `POST /clearances` records an immutable `clearance_id`,
   policy identity/version, environment/store, inclusive `from_sequence` and
   `through_sequence`, `financial_approval_ref`, `reconciliation_sha256` and
   reviewed `periods`. Each period includes `organization_id`, UTC
   `period_start`/`period_end`, `source_checkpoint_sha256`,
   `source_complete=true` and `reconciled=true`. The corresponding Billing
   period must also be closed. Closing a period does not create a clearance:
   authorized source-complete/late-event/commercial reconciliation attestation
   is separately required. `POST /clearances/{clearanceId}/revoke` stops future
   operations and retains immutable evidence.
4. The approved automatic controller `POST /operations` supplies the stable
   plan identity, full signed archive completion, fresh signed bounded
   `range_proof`, and exact ordered record bindings. Each operation covers at
   most 1,000 contiguous receipts; a full archive may contain more. Billing
   checks the signature against its approved public registry, exact
   environment/store/set/manifest/range/count, original record binding hash and
   trusted maximum receipt age. Full completion uses verification policy
   `billing-raw-v1`; fresh range proof binds the decimal retirement policy
   version. Financial policy rotation never changes an immutable completion.
5. Billing directly authenticates to every required configured consumer's
   `/v1/internal/billing-raw-lifecycle/reconcile` with the original record
   bindings. Caller-submitted consumer proofs or boolean complete claims are
   not accepted. Consumer identity, exact request/proof digests, range/count,
   committed frontier and a verification time within five minutes must match.
6. Under one PostgreSQL transaction-scoped store lock, Billing rechecks the
   policy, independent approval, clearance, signed age proof, consumer proofs,
   existing holds and contiguous prior retirement frontier, then durably
   records `ACTIVE`. No per-batch human approval is required after policy and
   financial period clearance have been authorized.

An old producer event gets a full hot interval based on trusted receipt time.
Missing/unknown evidence, young receipts, incomplete/late delivery, conflicting
digests, unavailable authority or a missing required consumer block retirement.
An empty outbox/page, high-water alone or a closed invoice never substitutes for
financial/source completeness. Clearance covers the reviewed immutable receipt
range; later receipts outside that range require their own clearance. Existing
facts rejected because a period is immutable cannot be bypassed to unlock cleanup.

## Holds and Durable Fencing

Financial authority `POST /holds` provides `hold_id`, environment/store,
inclusive receipt range, `reason` and `financial_approval_ref`. Both sequences
`"0"` mean a store-wide hold. A new hold ID is required to reopen a released
hold; identity and reason are immutable. `GET /holds/{holdId}` returns its state.

Holds that precede ACTIVE block the candidate. A new overlapping request after
ACTIVE is immediately persisted `PENDING_FENCED` and protects cloud archives and
key dependencies; it does not retroactively revoke the already decided local
batch. Finalization waits for the exact batch outcome. `POST
/holds/{holdId}/release` becomes `RELEASE_PENDING` while that fence is unresolved;
protection remains in force. Otherwise release is explicit and immediate.

The chosen ordering is a logical decision ordering, not a guarantee that a hold
arriving immediately before physical removal can cancel an in-flight decision.
Cloud retention has no automatic TTL; a held retired record can be manually
rehydrated. This protocol does not authorize cloud disposal.

One ACTIVE/ABORT_REQUESTED operation per logical store survives crashes:

```text
ACTIVE -> COMPLETED
       -> ABORT_REQUESTED -> COMPLETED | ABORTED
       -> ABORTED (durable participant abort proof)
```

Logger uses only the authority-read token for `GET /operations/{operationId}`.
Its apply must match ACTIVE, operation ID, environment/store, range, set and
plan digest. Logger apply/abort must serialize in the same retained database:
completed removal has a durable terminal receipt; abort has a permanent tombstone
that refuses every delayed apply. A status lookup or token expiry is not abort.

The controller `POST /operations/{operationId}/abort` only requests abort and
keeps the fence. It commands Logger abort separately, then `POST
/operations/{operationId}/resolve` with no terminal JSON. Billing itself reads
Logger `GET /v1/internal/billing-lifecycle/retire/{operationId}`, verifies the
exact immutable terminal receipt and sealed digest, and only then records
COMPLETED/ABORTED, releases the fence and finalizes queued holds. An unknown,
pending, inconsistent or unreachable outcome leaves the fence intact. Network
timeouts never automatically unlock it. Repeating an operation ID with the same
stable plan returns the original decision, including ABORTED; changed intent
conflicts and never resurrects it.

An empty abort body requires an existing operation. A controller may instead
provide the full stable plan to cancel a locally journaled intent that Billing
has never accepted. Under the same store lock used for admission, a missing row
becomes a permanent `ABORTED` tombstone with
`decision_origin=cancelled-before-acceptance`, no consumer proofs and **no
Logger terminal receipt**. This proves only that no ACTIVE decision existed in
the current authoritative history; it does not prove a Logger outcome or unlock
another operation. A racing admission either sees that tombstone and cannot
start, or wins first and must follow normal ABORT_REQUESTED/Logger reconciliation.
Changed intent conflicts; delayed commands cannot resurrect the ID. Unknown
history after restore must still be qualified before exposing the private API.

`GET /operations?environment=...&store_id=...` lists unresolved decisions for
the controller. Alert on their age, queued holds, unavailable provenance,
inconsistent terminal status and blocked cleanup. Recovery must reconcile
authority history with Logger tombstones, terminal receipts and retention floor
before allowing commands after snapshot restore. Unknown history is a hard
blocker; the four-hour restore-drill target does not authorize a forced unlock.
Online Logger compaction is independent of the authority's terminal decision.

## Stable Plan Digest

`PlanDigest` is lowercase SHA-256 of UTF-8 bytes:

```text
"rtk-billing-raw-retirement-plan-v1\x00" + json.Marshal(core)
```

`core` field order is `operation_id`, `policy_id`, integer `policy_version`,
`clearance_id`, `environment`, `store_id`, decimal-string `from_sequence`,
decimal-string `through_sequence`, `set_id`, `archive_manifest_sha256`,
`records_bindings_sha256`. The final field hashes `json.Marshal` of the ordered
array `{sequence:string, logger_content_sha256, usage_id, event_sha256}`. Fresh
verifier timestamps/signatures do not change the stable plan. Billing validates
the computed digest, not merely its hexadecimal shape; retained evidence is
not replaced on retry.

## Tests

Focused tests cover signed full-set/subrange provenance, trusted age, exact
consumer wire vectors, policy/recovery approvals, explicit source clearance,
hold/decision races, abort permanence, pending holds, immutable database guards,
unavailable participant outcomes and refusal of forged terminal JSON. PostgreSQL
tests require an isolated `TEST_DATABASE_URL`; run `go test -race` on
`internal/rawretention`, `internal/config`, `internal/api` and `cmd/server`.

Passing local tests is not live qualification. Activate automatic retirement
only after off-cluster verification, manual rehydration, receipt continuity,
matched recovery and all applicable cross-service acceptance gates pass.

## Maintenance Stop and Control-Plane Availability

`BILLING_RAW_RETENTION_ENABLED` enables the private authority control plane; it
is not a fence-cancellation switch. Setting it false makes the private listener
and all authority routes unavailable but preserves every durable decision. No
expiry or process restart releases an outstanding fence.

To stop new retirement safely:

1. Financial authority deactivates the approved policy with its exact hash.
   This blocks all new admission using that version, including still-running
   controllers. It does not retroactively revoke an accepted ACTIVE operation.
2. Keep the private listener, its network access and all dedicated credentials
   enabled. List unresolved operations and finish or request abort for each
   exact immutable ID, then reconcile the authenticated Logger terminal result.
   Exact existing-ID replay returns the original decision even while its policy
   is inactive; changed intent still conflicts. Abort and outcome reconciliation
   do not depend on current policy activation.
3. Disable the control plane or remove its credentials only after the
   authoritative unresolved-operation list is empty. Preserve cloud archives,
   unreleased holds, key dependencies and immutable history independently.

If the listener was stopped with outstanding fences, restore the same qualified
control plane and credentials to drain them. Never manufacture a terminal result
or erase the operation to recover availability. Unknown/rolled-back authority
history remains a recovery qualification blocker.
