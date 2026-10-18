"""Convert the protoc-gen-openapiv2 (Swagger 2.0) output to OpenAPI 3.0.3 YAML.

Usage: python scripts/openapi_v3.py <in.swagger.json> <out.yaml>

Only the Swagger features protoc-gen-openapiv2 emits are handled: definitions, path/query/body
parameters, responses with schemas, and `x-stream-definitions` for server streaming.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import yaml

STATUS_DESCRIPTIONS = {
    "400": "Invalid argument (with field violations)",
    "401": "Missing or invalid credentials",
    "403": "Role not allowed",
    "404": "Not found",
    "409": "Conflict (for example a conflicting label)",
    "429": "Rate limited",
    "504": "Deadline exceeded",
}


# Request constraints the server enforces (internal/api/validate.go, internal/api/cases.go). Swagger output from
# protoc-gen-openapiv2 carries none of them; tests/contract keeps the two in agreement. Character classes use
# [0-9] rather than \\d so generated data matches Go's RE2 semantics (no Unicode digits).
UUID = "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"
OCTET = "(25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])"
MCCS = ["4111", "4511", "4816", "4829", "5311", "5399", "5411", "5541", "5651", "5732", "5812", "5814", "5815", "5816", "5912", "5944", "5999", "7011", "7995"]  # schemas/events/transaction.json, D-051
CURRENCIES = [
    "AUD",
    "BRL",
    "CAD",
    "CHF",
    "DKK",
    "EUR",
    "GBP",
    "JPY",
    "MXN",
    "NOK",
    "NZD",
    "PLN",
    "SEK",
    "SGD",
    "USD",
]

SCHEMA_CONSTRAINTS: dict[str, dict[str, Any]] = {
    "v1ScoreRequest": {"required": ["transaction"], "additionalProperties": False},
    "v1Transaction": {
        "required": [
            "transaction_id",
            "account_id",
            "amount_minor",
            "currency",
            "mcc",
            "channel",
            "device_id",
            "ip_prefix",
            "country",
        ],
        "additionalProperties": False,
        "properties": {
            "transaction_id": {"pattern": UUID},
            "account_id": {"pattern": "^acc_[0-9a-z]{6,32}$"},
            "amount_minor": {"type": "integer", "format": "int64", "minimum": 1, "maximum": 10_000_000},
            "currency": {"enum": CURRENCIES},
            "merchant_id": {"pattern": "^(m_[0-9a-z]{4,32})?$"},  # optional: "" means absent in proto3
            "merchant_name": {"maxLength": 120, "pattern": "^[^\\x00-\\x1f\\x7f]*$"},
            "mcc": {"enum": MCCS},
            "channel": {
                "replace": {
                    "type": "string",
                    "enum": ["CHANNEL_CARD_PRESENT", "CHANNEL_ECOMMERCE", "CHANNEL_P2P"],
                }
            },
            "device_id": {"pattern": "^[0-9a-f]{64}$"},
            "ip_prefix": {"pattern": f"^{OCTET}\\.{OCTET}\\.{OCTET}\\.0/24$"},
            "country": {"pattern": "^[A-Z]{2}$"},
        },
    },
    "CaseServiceLabelCaseBody": {
        "required": ["label", "idempotency_key"],
        "additionalProperties": False,
        "properties": {
            "label": {"replace": {"type": "string", "enum": ["LABEL_FRAUD", "LABEL_LEGIT"]}},
            "note": {"maxLength": 2000, "pattern": "^[^\\x00]*$"},
            "idempotency_key": {"pattern": "^[A-Za-z0-9_-]{8,128}$"},
        },
    },
    "v1TestRuleRequest": {
        "required": ["when"],
        "additionalProperties": False,
        "properties": {
            "when": {"minLength": 1, "maxLength": 500},
            "sample_size": {"minimum": 0, "maximum": 5000},
        },
    },
}

# operationId -> parameter -> schema additions. 0 selects the documented default.
PARAM_CONSTRAINTS: dict[str, dict[str, dict[str, Any]]] = {
    "CaseService_ListCases": {
        "page_size": {"minimum": 0, "maximum": 100},
        "reason_code": {"pattern": "^[A-Z0-9_]{0,64}$"},
        "summary_status": {"enum": ["", "pending", "ok", "fallback", "unfaithful"]},
    },
    "CaseService_GetCase": {"case_id": {"pattern": UUID}},
    "CaseService_LabelCase": {"case_id": {"pattern": UUID}},
    "CaseService_GetTimeline": {
        "account_id": {"pattern": "^acc_[0-9a-z]{6,32}$"},
        "limit": {"minimum": 0, "maximum": 200},
    },
    "CaseService_GetShadowSummary": {
        "window_hours": {"minimum": 0, "maximum": 720},
        "disagreement_limit": {"minimum": 0, "maximum": 100},
    },
}


def apply_schema_constraints(schemas: dict[str, Any]) -> None:
    for name, extra in SCHEMA_CONSTRAINTS.items():
        schema = schemas[name]
        for key in ("required", "additionalProperties"):
            if key in extra:
                schema[key] = extra[key]
        for prop, add in extra.get("properties", {}).items():
            if "replace" in add:
                schema["properties"][prop] = add["replace"]
            else:
                schema["properties"][prop].update(add)


def rewrite_refs(node: Any) -> Any:
    if isinstance(node, dict):
        out = {}
        for key, value in node.items():
            if key == "$ref" and isinstance(value, str):
                out[key] = value.replace("#/definitions/", "#/components/schemas/")
            else:
                out[key] = rewrite_refs(value)
        return out
    if isinstance(node, list):
        return [rewrite_refs(v) for v in node]
    return node


def convert_operation(op: dict[str, Any], scorer: bool) -> dict[str, Any]:
    new: dict[str, Any] = {
        k: v for k, v in op.items() if k not in {"parameters", "responses", "consumes", "produces"}
    }
    params = []
    for param in op.get("parameters", []):
        if param.get("in") == "body":
            new["requestBody"] = {
                "required": bool(param.get("required", True)),
                "content": {"application/json": {"schema": rewrite_refs(param["schema"])}},
            }
            continue
        schema = {k: param[k] for k in ("type", "format", "enum", "items", "default") if k in param}
        p = {k: param[k] for k in ("name", "in", "required", "description") if k in param}
        schema.update(PARAM_CONSTRAINTS.get(op.get("operationId", ""), {}).get(param["name"], {}))
        p["schema"] = rewrite_refs(schema)
        if param.get("type") == "array":
            p["style"] = "form"
            p["explode"] = True
        params.append(p)
    if params:
        new["parameters"] = params
    responses: dict[str, Any] = {}
    for code, resp in op.get("responses", {}).items():
        r: dict[str, Any] = {"description": resp.get("description") or "OK"}
        if "schema" in resp:
            r["content"] = {"application/json": {"schema": rewrite_refs(resp["schema"])}}
        responses[code] = r
    error_schema = {"$ref": "#/components/schemas/rpcStatus"}
    for code, desc in STATUS_DESCRIPTIONS.items():
        responses.setdefault(
            code, {"description": desc, "content": {"application/json": {"schema": error_schema}}}
        )
    new["responses"] = responses
    new["security"] = [{"serviceToken": []}] if scorer else [{"bearerJWT": []}]
    return new


def convert(swagger: dict[str, Any]) -> dict[str, Any]:
    paths: dict[str, Any] = {}
    for path, item in swagger.get("paths", {}).items():
        paths[path] = {
            method: convert_operation(op, scorer=path == "/v1/score") for method, op in item.items()
        }
    schemas = rewrite_refs(swagger.get("definitions", {}))
    for name, defn in swagger.get("x-stream-definitions", {}).items():
        schemas[name] = rewrite_refs(defn)
    apply_schema_constraints(schemas)
    return {
        "openapi": "3.0.3",
        "info": {
            "title": "fraud-scoring-service API",
            "version": "v1",
            "description": "Generated from proto/fraud/v1 by make proto. Do not edit.",
            "license": {"name": "MIT"},
        },
        "servers": [{"url": "http://localhost:8511"}],
        "tags": swagger.get("tags", []),
        "paths": paths,
        "components": {
            "schemas": schemas,
            "securitySchemes": {
                "serviceToken": {"type": "http", "scheme": "bearer", "description": "FRAUD_SERVICE_TOKEN"},
                "bearerJWT": {"type": "http", "scheme": "bearer", "bearerFormat": "JWT"},
            },
        },
    }


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    swagger = json.loads(Path(argv[1]).read_text(encoding="utf-8"))
    out = convert(swagger)
    Path(argv[2]).write_text(
        "# Code generated by scripts/openapi_v3.py. DO NOT EDIT.\n"
        + yaml.safe_dump(out, sort_keys=True, width=120),
        encoding="utf-8",
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
