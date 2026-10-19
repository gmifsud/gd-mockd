#!/usr/bin/env python3
"""Build OpenAPI 3.0 specs for the Wolt digital twin from a captured contract snapshot.

Wolt does not publish downloadable OpenAPI files; its reference pages at
https://developer.wolt.com/docs/api/* are rendered from OpenAPI internally.
The community project https://github.com/Nirbel/wolt-mcp (MIT) captures those
pages into `specs/wolt-official.json` (operations, parameters, request schemas,
response schemas and Wolt's own example payloads, with source URLs and a
capture date). This script converts that snapshot into one OpenAPI 3.0.3 file
per Wolt API so that `mockd` can import them.

Normalisation applied (JSON Schema 2020-12 -> OpenAPI 3.0, which mockd parses):
  * `examples: [a, ...]`         -> `example: a`
  * `examples: {name: {value}}`  -> moved to the media type as `example`/`examples`
  * `type: [T, "null"]`          -> `type: T, nullable: true`
  * `$schema`, `x-readme-ref-name` dropped (ref name kept as `title`)
  * path params -> `in: path` parameters, body params -> `requestBody`

Usage:
  python3 -I tools/build_specs.py <wolt-official.json> <out-dir>
"""

import json
import re
import sys
from pathlib import Path

# Snapshot source key -> (output file stem, mockd import namespace hint)
APIS = {
    "order": "order",
    "menu": "menu",
    "venue": "venue",
    "timeslot": "timeslot",
    "drive": "drive",
    # "order-submitter" has no callable paths (it is an inbound Wolt -> partner
    # payload); its example is extracted separately for the simulator.
}


def normalise_schema(node):
    """Recursively convert a JSON Schema 2020-12 fragment to OpenAPI 3.0."""
    if isinstance(node, list):
        return [normalise_schema(n) for n in node]
    if not isinstance(node, dict):
        return node

    out = {}
    for key, value in node.items():
        if key in ("$schema",):
            continue
        if key == "x-readme-ref-name":
            out.setdefault("title", value)
            continue
        if key == "examples" and isinstance(value, list):
            if value:
                out["example"] = value[0]
            continue
        if key == "examples" and isinstance(value, dict):
            # Named examples belong on the media type; handled by caller.
            out["x-named-examples"] = value
            continue
        if key == "type" and isinstance(value, list):
            non_null = [t for t in value if t != "null"]
            out["type"] = non_null[0] if non_null else "string"
            if "null" in value:
                out["nullable"] = True
            continue
        if key == "properties" and isinstance(value, dict):
            out[key] = {k: normalise_schema(v) for k, v in value.items()}
            continue
        out[key] = normalise_schema(value)
    return out


def media_type(schema):
    """Build an OpenAPI media type object, lifting named examples off the schema."""
    schema = normalise_schema(schema)
    mt = {}
    named = schema.pop("x-named-examples", None)
    if named:
        mt["examples"] = named
        first = next(iter(named.values()))
        if isinstance(first, dict) and "value" in first:
            mt["example"] = first["value"]
    mt["schema"] = schema
    return mt


LOST_CHUNK = re.compile(r"^\$[0-9a-f]+$")  # RSC text chunk that was not captured


def is_backref(value):
    return isinstance(value, str) and value.startswith("$") and ":operations:" in value


def lookup_backref(ref, operations):
    """Resolve a React-flight back-reference left in the captured page data.

    Shape: "$8:props:...:operations:<opId>:operation:<path...>" where <path> walks
    the original OpenAPI operation (responses/<code>/content/<ct>/schema/...,
    or requestBody/... which maps to the captured body parameter's schema).
    The page de-duplicates repeated sub-schemas this way.
    """
    tail = ref.split(":operations:", 1)[1]
    op_id, path = tail.split(":operation:", 1)
    node = operations[op_id]
    segments = path.split(":")
    if segments[0] == "requestBody":
        node = next(p["schema"] for p in node["parameters"] if p.get("type") == "body")
        segments = segments[1:]
    for seg in segments:
        node = node[int(seg)] if isinstance(node, list) else node[seg]
    return node


def resolve_backrefs(node, operations, depth=0):
    """Replace every back-reference string in `node` with a deep copy of its target."""
    if depth > 50:
        raise SystemExit("back-reference resolution did not converge")
    if is_backref(node):
        return resolve_backrefs(json.loads(json.dumps(lookup_backref(node, operations))), operations, depth + 1)
    if isinstance(node, dict):
        return {k: resolve_backrefs(v, operations, depth) for k, v in node.items()}
    if isinstance(node, list):
        return [resolve_backrefs(v, operations, depth) for v in node]
    if isinstance(node, str) and LOST_CHUNK.match(node):
        return "(description not captured in snapshot; see externalDocs)"
    return node


def convert_operation(op_id, op):
    params = []
    request_body = None
    for p in op.get("parameters") or []:
        schema = p.get("schema") or {}
        if p.get("type") == "path":
            for name, prop in (schema.get("properties") or {}).items():
                prop = normalise_schema(prop)
                param = {
                    "name": name,
                    "in": "path",
                    "required": True,  # OpenAPI requires path params to be required
                    "schema": {k: v for k, v in prop.items() if k != "description"},
                }
                if prop.get("description"):
                    param["description"] = prop["description"]
                params.append(param)
        elif p.get("type") == "body":
            request_body = {
                "required": True,
                "content": {"application/json": media_type(schema)},
            }
            if p.get("description"):
                request_body["description"] = p["description"]
        else:
            raise SystemExit(f"{op_id}: unsupported parameter location {p.get('type')!r}")

    responses = {}
    for code, resp in (op.get("responses") or {}).items():
        r = {"description": resp.get("description") or str(code)}
        if resp.get("content"):
            r["content"] = {ct: media_type(mt.get("schema") or {}) for ct, mt in resp["content"].items()}
        responses[str(code)] = r

    out = {
        "operationId": op_id,
        "summary": op.get("summary") or op_id,
        "description": op.get("description") or "",
        "security": [{"bearerAuth": []}],
        "responses": responses,
    }
    if params:
        out["parameters"] = params
    if request_body:
        out["requestBody"] = request_body
    return op["method"].lower(), op["path"], out


def build(snapshot_path, out_dir):
    snap = json.loads(Path(snapshot_path).read_text())
    captured_at = snap.get("captured_at", "unknown")
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    for key, stem in APIS.items():
        src = snap["sources"][key]
        operations = resolve_backrefs(src["operations"], src["operations"])
        paths = {}
        for op_id, op in operations.items():
            method, path, operation = convert_operation(op_id, op)
            paths.setdefault(path, {})[method] = operation
        spec = {
            "openapi": "3.0.3",
            "info": {
                "title": f"Wolt {src['title']}",
                "version": captured_at,
                "description": (
                    f"Generated from the official Wolt reference at {src['url']} "
                    f"(snapshot captured {captured_at} by github.com/Nirbel/wolt-mcp). "
                    "Do not hand-edit; regenerate with tools/build_specs.py."
                ),
            },
            "externalDocs": {"url": src["url"]},
            "servers": src.get("servers") or [],
            "components": {
                "securitySchemes": {
                    "bearerAuth": {"type": "http", "scheme": "bearer", "bearerFormat": "JWT"}
                }
            },
            "paths": dict(sorted(paths.items())),
        }
        target = out_dir / f"{stem}.openapi.json"
        target.write_text(json.dumps(spec, indent=2, ensure_ascii=False) + "\n")
        print(f"wrote {target} ({sum(len(v) for v in paths.values())} operations)")

    # Order Submitter: inbound payload example for the simulator.
    sub = snap["sources"]["order-submitter"]["operations"]["get-notifications"]
    sub_schema = sub["responses"]["200"]["content"]["application/json"]["schema"]
    example = sub_schema["examples"]["default"]["value"]
    target = out_dir / "order-submitter.example.json"
    target.write_text(json.dumps(example, indent=2, ensure_ascii=False) + "\n")
    print(f"wrote {target}")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit(__doc__)
    build(sys.argv[1], sys.argv[2])
