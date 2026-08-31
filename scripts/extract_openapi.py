#!/usr/bin/env python3
"""Import Billing paths from a compatibility OpenAPI while preserving RTK Billing boundaries.

The checked-in ``openapi.yaml`` is authoritative. This migration helper remains
for reviewing older compatibility documents and must never collapse tenant,
internal, and debit credentials into one security scheme.
"""

from __future__ import annotations

import re
import sys
import copy
from pathlib import Path

import yaml


PREFIXES = (
    "/v1/orgs/{orgId}/billing/",
    "/v1/orgs/{orgId}/payment-methods",
    "/v1/orgs/{orgId}/auto-topup",
    "/v1/orgs/{orgId}/topups",
    "/v1/orgs/{orgId}/payment-intents",
    "/v1/internal/billing/",
    "/v1/internal/payment-simulator/",
    "/v1/payment-webhooks/",
)

OPERATION_MAPPINGS = {
    "getBillingAccess": ("FEAT-BILL-SERVICE-001", ["REQ-BILL-ACCESS-001"]),
    "putBillingAccess": ("FEAT-BILL-SERVICE-001", ["REQ-BILL-ACCESS-001"]),
    "createBillingPricingVersion": ("FEAT-AM-INVOICE-001", ["REQ-AM-PRICING-VERSION-001"]),
    "activateBillingPricingVersion": ("FEAT-AM-INVOICE-001", ["REQ-AM-PRICING-VERSION-001"]),
    "putBillingUsageFact": ("FEAT-AM-INVOICE-001", ["REQ-AM-INVOICE-LIFECYCLE-001", "REQ-AM-INVOICE-ARITHMETIC-001"]),
    "closeBillingPeriod": ("FEAT-AM-INVOICE-001", ["REQ-AM-INVOICE-LIFECYCLE-001", "REQ-AM-INVOICE-ARITHMETIC-001"]),
    "listBillingInvoices": ("FEAT-AM-INVOICE-001", ["REQ-AM-INVOICE-LIFECYCLE-001"]),
    "getBillingInvoice": ("FEAT-AM-INVOICE-001", ["REQ-AM-INVOICE-LIFECYCLE-001"]),
    "downloadBillingInvoicePdf": ("FEAT-AM-INVOICE-001", ["REQ-AM-INVOICE-DOCUMENT-001"]),
    "getBillingSummary": ("FEAT-AM-INVOICE-001", ["REQ-AM-BILLING-SUMMARY-001"]),
    "getBillingUsage": ("FEAT-AM-INVOICE-001", ["REQ-AM-BILLING-SUMMARY-001"]),
    "getBillingProfile": ("FEAT-AM-INVOICE-001", ["REQ-AM-BILLING-PROFILE-001"]),
    "putBillingProfile": ("FEAT-AM-INVOICE-001", ["REQ-AM-BILLING-PROFILE-001"]),
    "exportBillingStatement": ("FEAT-AM-INVOICE-001", ["REQ-AM-INVOICE-DOCUMENT-001"]),
    "listBillingActivity": ("FEAT-AM-BILLING-ACTIVITY-001", ["REQ-AM-BILLING-ACTIVITY-001", "REQ-AM-BILLING-ACTIVITY-002", "REQ-AM-BILLING-ACTIVITY-003"]),
    "getBillingActivity": ("FEAT-AM-BILLING-ACTIVITY-001", ["REQ-AM-BILLING-ACTIVITY-001", "REQ-AM-BILLING-ACTIVITY-002", "REQ-AM-BILLING-ACTIVITY-003"]),
}


def component_refs(value: object) -> set[tuple[str, str]]:
    refs: set[tuple[str, str]] = set()
    if isinstance(value, dict):
        ref = value.get("$ref")
        if isinstance(ref, str):
            match = re.fullmatch(r"#/components/([^/]+)/([^/]+)", ref)
            if match:
                refs.add((match.group(1), match.group(2)))
        for child in value.values():
            refs.update(component_refs(child))
    elif isinstance(value, list):
        for child in value:
            refs.update(component_refs(child))
    return refs


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit("usage: extract_openapi.py SOURCE OUTPUT")
    source = yaml.safe_load(Path(sys.argv[1]).read_text())
    paths = {key: value for key, value in source["paths"].items() if key.startswith(PREFIXES)}
    access_schema = {
        "type": "object",
        "required": ["organization_id", "state", "version", "updated_by", "created_at", "updated_at"],
        "properties": {
            "organization_id": {"type": "string", "format": "uuid"},
            "state": {"type": "string", "enum": ["active", "read_only", "suspended", "closed"]},
            "reason_code": {"type": "string"},
            "version": {"type": "integer", "minimum": 1},
            "updated_by": {"type": "string"},
            "created_at": {"type": "string", "format": "date-time"},
            "updated_at": {"type": "string", "format": "date-time"},
        },
        "additionalProperties": False,
    }
    paths["/v1/internal/billing/access/{orgId}"] = {
        "parameters": [{"name": "orgId", "in": "path", "required": True, "schema": {"type": "string", "format": "uuid"}}],
        "get": {
            "operationId": "getBillingAccess", "summary": "Read the Billing-owned commercial access state",
            "security": [{"billingServiceAuth": []}],
            "responses": {"200": {"description": "Current access state", "content": {"application/json": {"schema": {"type": "object", "properties": {"access": access_schema}}}}}},
        },
        "put": {
            "operationId": "putBillingAccess", "summary": "Update commercial access using optimistic concurrency",
            "security": [{"billingServiceAuth": []}],
            "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object", "required": ["state", "version"], "properties": {"state": copy.deepcopy(access_schema["properties"]["state"]), "reason_code": {"type": "string"}, "version": {"type": "integer", "minimum": 1}}, "additionalProperties": False}}}},
            "responses": {"200": {"description": "Updated access state"}, "409": {"description": "Version conflict"}},
        },
    }
    for path, item in paths.items():
        for method, operation in item.items():
            if method.lower() not in {"get", "put", "post", "patch", "delete"}:
                continue
            if "payment-webhooks" in path or "payment-simulator/setup-callback" in path:
                operation["security"] = []
            elif path == "/v1/internal/billing/debits":
                operation["security"] = [{"billingDebitAuth": []}]
            elif path.startswith("/v1/internal/billing/"):
                operation["security"] = [{"billingInternalAuth": []}]
            else:
                operation["security"] = [{"billingServiceAuth": []}]
            operation_id = operation.get("operationId")
            if operation_id in OPERATION_MAPPINGS:
                feature_id, requirement_ids = OPERATION_MAPPINGS[operation_id]
                operation["x-rtk-feature-id"] = feature_id
                operation["x-rtk-requirement-ids"] = requirement_ids

    components: dict[str, dict[str, object]] = {"securitySchemes": {
        "billingServiceAuth": {
            "type": "http", "scheme": "bearer", "bearerFormat": "service-token",
            "description": "Dedicated Cloud Admin-to-Billing tenant API credential. Calls also require X-Billing-Actor-Type=user, a global Account Manager user ID in X-Billing-Actor-ID, X-Billing-Permissions, and X-Request-ID. The retired brand_cloud_user actor type is rejected; historical audit actors are not rewritten.",
        },
        "billingInternalAuth": {
            "type": "http", "scheme": "bearer", "bearerFormat": "internal-service-token",
            "description": "Dedicated trusted usage, pricing, period-close, and access-control credential.",
        },
        "billingDebitAuth": {
            "type": "http", "scheme": "bearer", "bearerFormat": "debit-source-token",
            "description": "Dedicated debit producer credential, distinct from tenant and other internal credentials.",
        },
    }}
    pending = component_refs(paths)
    copied: set[tuple[str, str]] = set()
    while pending:
        section, name = pending.pop()
        if (section, name) in copied:
            continue
        value = source.get("components", {}).get(section, {}).get(name)
        if value is None:
            raise SystemExit(f"missing component {section}/{name}")
        components.setdefault(section, {})[name] = value
        copied.add((section, name))
        pending.update(component_refs(value) - copied)

    document = {
        "openapi": "3.1.0",
        "x-rtk-spec": {"id": "SPEC-BILLING-OPENAPI", "status": "normative", "owner": "rtk_billing"},
        "info": {"title": "RTK Billing API", "version": "1.0.0", "description": "Provider-neutral pricing, wallet, payment, invoice, and billing access service."},
        "paths": paths,
        "components": components,
    }
    Path(sys.argv[2]).write_text(yaml.safe_dump(document, sort_keys=False, allow_unicode=True))


if __name__ == "__main__":
    main()
