#!/usr/bin/env python3
"""Generate the standalone Billing OpenAPI from the compatibility source.

This is temporary migration tooling. Once Account Manager removes its legacy
surface, the generated file in this repository becomes the editing source.
"""

from __future__ import annotations

import re
import sys
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
            "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object", "required": ["state", "version"], "properties": {"state": access_schema["properties"]["state"], "reason_code": {"type": "string"}, "version": {"type": "integer", "minimum": 1}}, "additionalProperties": False}}}},
            "responses": {"200": {"description": "Updated access state"}, "409": {"description": "Version conflict"}},
        },
    }
    for path, item in paths.items():
        for method, operation in item.items():
            if method.lower() not in {"get", "put", "post", "patch", "delete"}:
                continue
            operation["security"] = [] if "payment-webhooks" in path or "payment-simulator/setup-callback" in path else [{"billingServiceAuth": []}]

    components: dict[str, dict[str, object]] = {"securitySchemes": {
        "billingServiceAuth": {
            "type": "http", "scheme": "bearer", "bearerFormat": "service-token",
            "description": "Dedicated Cloud Admin or trusted billing-worker service credential.",
        }
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
        "info": {"title": "RTK Billing API", "version": "1.0.0", "description": "Provider-neutral pricing, wallet, payment, invoice, and billing access service."},
        "paths": paths,
        "components": components,
    }
    Path(sys.argv[2]).write_text(yaml.safe_dump(document, sort_keys=False, allow_unicode=True))


if __name__ == "__main__":
    main()
