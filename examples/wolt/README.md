# Wolt digital twin

A local, stateful stand-in for Wolt's partner integration APIs, built on mockd. Point a POS, middleware or delivery integration at it instead of Wolt's development environment to run end-to-end tests offline, repeatably, and with failure modes on demand.

| Wolt surface | Covered by | Behaviour |
|---|---|---|
| Authentication 2.0 (`integrations-authentication-service`) | mockd OAuth provider | Authorization-code flow (1 h single-use codes), HTTP Basic client auth, 1 h RS256 access tokens carrying `integration.venue_id`, **single-use** 30-day refresh tokens; refresh revokes the previous access token and refresh-token reuse revokes the newly issued pair |
| Order API v1 + v2 (`pos-integration-service`) | tables + custom operations | Full lifecycle with Wolt's transition rules, both payload versions kept in sync |
| Menu API | tables + custom operations | Menu push, async menu export (`resource_url` polling), item/inventory/option updates with identifier validation |
| Venue API | tables + custom operations | Status, online/offline, opening hours, special hours, settings, delivery provider |
| Time slot API | imported contract | Wolt's example responses (stateless) |
| Wolt Drive (`daas-public-api`) | tables + custom operations | Shipment promises → deliveries, delivery-area and promise checks, duplicate detection, cancellation |
| Order webhooks, Order Submitter, venue alerts, Drive webhooks | `sim/wolt_sim.py` | Signed callbacks to your endpoint, plus Wolt-side behaviour (couriers, auto-reject, pre-orders) |

## How it fits together

```text
                         ┌────────────── mockd (one process) ──────────────┐
  partner system  ─────► │ :4280  /oauth2/*  /orders/*  /v2/orders/*       │
  (POS / middleware)     │        /venues/*  /v1/restaurants/*  /v1/venues/*│
        ▲                │        /merchants/*  /order/*                   │
        │                │   ── custom operations enforce Wolt's rules ──  │
        │                │   ── state: orders, venues, menus, deliveries ──│
        │                │   ── event log: order/venue/drive_events ────── │
        │                │ :4290  admin API (state, ops, request log)      │
        │                └───────────────────────┬─────────────────────────┘
        │   WOLT-SIGNATURE webhooks,             │ polls event log,
        │   Order Submitter pushes,              │ runs Wolt-side operations
        │   Drive JWT webhooks                   ▼
        └──────────────────────────── sim/wolt_sim.py follow --auto
```

Every partner call that changes state appends to an event log table. `wolt_sim.py follow` turns those events into the webhooks Wolt would send and, with `--auto`, performs the transitions Wolt owns (courier pickup and drop-off, auto-reject, starting confirmed pre-orders, Drive courier progress).

## Quick start

```bash
# 1. Build mockd from this repo (or use an installed release that includes the changes in this branch)
go build -o mockd ./cmd/mockd

# 2. Start the twin
./mockd start -c examples/wolt/mockd.yaml --no-auth        # add -d to run in the background

# 3. Deliver webhooks to your integration (second terminal)
python3 examples/wolt/sim/wolt_sim.py follow --auto \
  --webhook-url http://localhost:3000/wolt/webhook \
  --drive-webhook-url http://localhost:3000/wolt/drive-webhook

# 4. Place an order as Wolt would
python3 examples/wolt/sim/wolt_sim.py new-order --kind delivery
```

Reset all state to the fixtures at any time: `curl -X POST http://localhost:4290/state/reset`.

### Point your integration at the twin

| Setting | Wolt (development) | Twin |
|---|---|---|
| Token endpoint | `https://integrations-authentication-service.development.dev.woltapi.com/oauth2/token` | `http://localhost:4280/oauth2/token` |
| Authorize (onboarding redirect) | issued by Wolt onboarding | `http://localhost:4280/oauth2/authorize` |
| Marketplace APIs (Order, Menu, Venue, Time slot) | `https://pos-integration-service.development.dev.woltapi.com` | `http://localhost:4280` |
| Wolt Drive | `https://daas-public-api.development.dev.woltapi.com` | `http://localhost:4280` |
| `client_id` / `client_secret` | issued by Wolt | `wolt-pilot-client` / `wolt-pilot-secret` |
| Webhook signing secret | your client secret | `wolt-pilot-secret` (`--webhook-secret`) |
| Drive webhook JWT secret | registered per webhook | `wolt-drive-secret` (`--drive-webhook-secret`) |
| Drive Merchant Key | issued by Wolt | any bearer token |

Allowed OAuth redirect URIs are listed under `mocks[wolt-auth].oauth.clients` in `mockd.yaml`; add yours there.

**Getting a token** (the authorize step stands in for Wolt's merchant onboarding and auto-approves the venue named by `venue_id`; seeded venues are `68656c6c6f76326f72646572` and `wolt-pilot-venue-b`):

```bash
curl -si "http://localhost:4280/oauth2/authorize?client_id=wolt-pilot-client&redirect_uri=http://localhost:3000/wolt/oauth/callback&response_type=code&state=abc&venue_id=68656c6c6f76326f72646572" | grep -i location
# Location: http://localhost:3000/wolt/oauth/callback?code=<code>&state=abc

curl -s -u wolt-pilot-client:wolt-pilot-secret http://localhost:4280/oauth2/token \
  -d grant_type=authorization_code -d code=<code> -d redirect_uri=http://localhost:3000/wolt/oauth/callback
# {"access_token":"eyJ...","token_type":"Bearer","expires_in":3600,"refresh_token":"...","scope":"offline"}

curl -s -u wolt-pilot-client:wolt-pilot-secret http://localhost:4280/oauth2/token \
  -d grant_type=refresh_token -d refresh_token=<refresh_token>
# returns a NEW refresh_token and access token; the previous access token stops working.
# Replaying a consumed refresh token fails with invalid_grant AND revokes the newest pair
# (Wolt: "reusing the same refresh token ... will immediately deactivate and revoke the
# newly issued access and refresh tokens"), so the venue must be onboarded again.
```

**Guarded endpoints.** `PATCH /venues/{venueId}/items` and `PATCH /venues/{venueId}/items/inventory` require a `wolt-auth` access token (valid signature, issuer and expiry, not revoked) whose `integration.venue_id` equals `{venueId}`. Missing or invalid tokens get `401 {"detail":"Unauthorized"}`; another venue's token gets `403 {"detail":"Forbidden"}`. Both are checked before validation, so a rejected request never changes state. To demo without tokens, set `mode: warn` on `x-wolt-venue-bearer` (violations are logged, not rejected); never do this in acceptance tests.

The other endpoints are not guarded yet and accept any `Authorization: Bearer <token>`; there, `Bearer expired-token` yields `401 {"detail":"Unauthorized"}`. `wolt_sim.py audit` fails if any captured Wolt API request lacked a bearer token.

## Fixtures

| Id | What |
|---|---|
| `68656c6c6f76326f72646572` | Marketplace venue "Wolt's Cafe" (`external_venue_id` `wolt_external_store`), online, menu seeded |
| `6710a1000000000000000001` | Order — instant, Wolt courier delivery, `received` |
| `6710a1000000000000000002` | Order — takeaway, `received` |
| `6710a1000000000000000003` | Order — pre-order, `pre_order_status: waiting` |
| `6710a1000000000000000004` | Order — self-delivery, `received` |
| `6710a1000000000000000005` | Order — delivered yesterday (refunds, terminal-state checks) |
| `6710d0000000000000000001` | Wolt Drive venue (Helsinki centre; delivery area ±0.15° lat / ±0.30° lon) |

Order bodies are Wolt's own reference examples. `wolt_sim.py new-order --kind delivery|takeaway|preorder|self-delivery` clones a fixture into a fresh order with current timestamps.

## Order lifecycle

```text
                 accept / self-delivery/accept            ready              delivered*
 received ─────────────────────────────────► production ──────► ready ──────────────► delivered
    │  └── confirm-preorder (pre-orders) ── Wolt starts production ──┘   (*Wolt courier: done by Wolt)
    │
    └── reject (reason required) ──► rejected      Wolt-side cancel / auto-reject from any non-final state
```

| Call | Allowed when | Otherwise | Success | Webhook |
|---|---|---|---|---|
| `PUT /orders/{id}/accept` `{adjusted_pickup_time?}` | `received`/`fetched`/`acknowledged`, not self-delivery; adjusted time ≤ 25 min after the pickup estimate | 409 / 400 | 202 | `PRODUCTION` |
| `PUT /orders/{id}/self-delivery/accept` | self-delivery, before acceptance | 409 | 202 | `PRODUCTION` |
| `PUT /orders/{id}/reject` `{reason, code?}` | before acceptance; `code` ∈ GENERIC, ITEMS_UNAVAILABLE, VENUE_CLOSING_SOON | 409 / 400 | 202 | `CANCELED` |
| `PUT /orders/{id}/confirm-preorder` | pre-order with `pre_order_status: waiting` | 409 | 204 | — (then `PRODUCTION` when Wolt starts it) |
| `PUT /orders/{id}/ready` | `production` | 409 | 202 | `READY` |
| `PUT /orders/{id}/delivered` | `ready` | 409 | 202 | `DELIVERED` |
| `PUT /orders/{id}/pickup-completed`, `/courier-at-customer` | self-delivery and `ready` | 409 | 202 | optional |
| `PUT /orders/{id}/delivery/tracking/location` `{location}` | self-delivery and `ready` | 409 / 400 | 202 | — |
| `PUT /orders/{id}/delivery/eta` `{eta_minutes}` | self-delivery | 409 / 400 | 202 | — |
| `PUT /orders/{id}/replace-items` `{item_changes}` | `production` | 409 / 400 | 202 | — |
| `POST /orders/{id}/refund-items` `{reason, item_refunds}` | after acceptance, not rejected | 409 / 400 | 202 | — |
| `POST /orders/{id}/refund-basket` `{basket_refunds: [one]}` | after acceptance, not rejected | 409 / 400 | 202 | — |
| `PUT /orders/{id}/sent-to-pos`, `/deposits-returned` | any | 400 on missing fields | 204 / 202 | — |
| `GET /orders/{id}`, `GET /v2/orders/{id}` | — | 404 | 200 | — |

Unknown orders return 404. Marketplace error bodies are `{"detail": "<reason>"}`.

### Webhooks (`wolt_sim.py follow`)

```json
POST <webhook-url>
WOLT-SIGNATURE: <hex HMAC-SHA256 of the raw body with the client secret>

{"id":"<uuid>","type":"order.notification",
 "order":{"id":"…","venue_id":"…","status":"CREATED","resource_url":"http://localhost:4280/orders/…"},
 "created_at":"2026-10-19T10:00:00.000Z"}
```

- Retries up to 3 times, 5 s apart, until your endpoint answers 200 (`--retries`, `--retry-delay`).
- `--order-mode submitter` pushes the full order (Order Submitter) instead of a notification, with the same signature header.
- `--optional-events` adds `COURIER ARRIVAL` and `PICK-UP-COMPLETED`.
- `--auto` plays Wolt: courier arrival after acceptance, pickup and delivery after `ready` (`--courier-delay`), pre-order production after confirmation (`--preorder-delay`), and `--auto-reject-after N` cancels orders not accepted within N seconds (Wolt's real window is minutes; the twin defaults to off).
- Venue changes to opening hours send `OPENING_HOURS_UPDATED`; send other venue alerts with `wolt_sim.py venue-event VENUE_OFFLINE_ALERT_TRIGGERED` (also `REJECTION_ALERT_TRIGGERED`, `…_RECOVERED`).
- Wolt-side actions on demand: `wolt_sim.py wolt cancel|courier-arrived|picked-up|courier-at-customer|deliver|preorder-production <order-id>`.

## Menu and Venue API

| Call | Behaviour |
|---|---|
| `POST /v1/restaurants/{venueId}/menu` | Stores the menu (requires `categories`); 202 |
| `GET /v2/venues/{venueId}/menu` | 202 `{request_id, resource_url}`; GET the `resource_url` for `{request_id, status: "READY", error, menu}` |
| `PATCH /venues/{venueId}/items` | Each entry needs exactly one of `external_id` / `gtin` / `sku`; 202 |
| `PATCH /venues/{venueId}/items/inventory` | As above, and `inventory` must not be null; 202 |
| `PATCH /venues/{venueId}/options/values` | Each entry needs `external_id`; 202 |
| `GET /venues/{venueId}/status` | Wolt's status document; reflects online/opening-hours changes |
| `PATCH /venues/{venueId}/online` `{status: ONLINE\|OFFLINE, until?}` | Updates `status.is_online`; 204 |
| `PATCH /venues/{venueId}/opening-times` `{availability}` | Replaces `opening_times`; 202 + `OPENING_HOURS_UPDATED` webhook |
| `PUT /venues/{venueId}/special-opening-times` | Recorded; 202 + `OPENING_HOURS_UPDATED` webhook |
| `GET /venues/{venueId}/settings`, `GET`/`PATCH …/delivery-provider` | Read and switch `WOLT` ↔ `SELF_DELIVERY` |

Menu price and inventory updates now change the stored menu and are recorded in `menu_events`
(`GET http://localhost:4290/state/resources/menu_events/items`). Each accepted event includes
`matched`, `unmatched`, and per-input `results`; unknown identifiers do not create items.
Rejected batches leave the catalogue unchanged and remain visible in the HTTP request journal.
See [Increment B handoff](INCREMENT-B-HANDOFF.md) for the bounded field support and WSL checks.

## Wolt Drive

```bash
# 1. Promise (binding only when lat/lon are given)
curl -s -X POST localhost:4280/v1/venues/6710d0000000000000000001/shipment-promises -H 'Authorization: Bearer k' \
  -H 'Content-Type: application/json' \
  -d '{"lat":60.168,"lon":24.94,"street":"Mannerheimintie 1","city":"Helsinki","post_code":"00100","min_preparation_time_minutes":10}'
# 2. Delivery from the promise
curl -s -X POST localhost:4280/v1/venues/6710d0000000000000000001/deliveries -H 'Authorization: Bearer k' \
  -H 'Content-Type: application/json' -d '{"shipment_promise_id":"<id>","merchant_order_reference_id":"POS-1001",
  "recipient":{"name":"Yuho","phone_number":"+3581234567"},"parcels":[{"count":1,"description":"Bag"}],
  "dropoff":{"location":{"coordinates":{"lat":60.168,"lon":24.94}},"comment":"Door code 1234"}}'
```

| Condition | Response |
|---|---|
| Unknown venue | 404 `VENUE_NOT_FOUND` |
| Drop-off outside the venue's area | 400 `DROPOFF_OUTSIDE_OF_DELIVERY_AREA` |
| `scheduled_dropoff_time` in the past | 400 `INVALID_SCHEDULED_DROPOFF_TIME` |
| `cash` present (cash disabled) | 400 `CASH_ON_DELIVERY_DISABLED` |
| Unknown, expired (10 min) or other-venue promise | 400 `SHIPMENT_PROMISE_NOT_FOUND` |
| Non-binding promise, or missing required fields | 400 `INVALID_PAYLOAD` |
| Reused `merchant_order_reference_id` | 400 `DUPLICATE_ORDER` |
| `PATCH /order/{wolt_order_reference_id}/status/cancel` after courier pickup started | 409 `CANCELLATION_NOT_ALLOWED` |

Errors use Drive's documented envelope `{"error_code", "reason", "details"}`. With `follow --auto`, each delivery emits `order.received`, `order.pickup_started`, `order.pickup_arrival`, `order.picked_up`, `order.dropoff_started`, `order.dropoff_arrival`, `order.dropoff_completed`, `order.delivered` to `--drive-webhook-url` as `{"token": "<HS256 JWT>"}`.

The venueless Drive endpoints (`/merchants/{merchantId}/available-venues`, `/delivery-fee`, `/delivery-order`) and the Time slot API serve Wolt's contract with schema-generated example values only.

## Fault injection

Path-scoped faults go through the admin API (rule `probability` must be set; it defaults to 0):

```bash
# Wolt rate-limits menu item updates: 429 with Retry-After, recovering after 60 s
# Order API: half the calls fail with 503
curl -X PUT localhost:4290/chaos -H 'Content-Type: application/json' -d '{
  "enabled": true,
  "rules": [
    {"pathPattern": "^/venues/.*/items", "probability": 1,
     "faults": [{"type": "retry_after", "probability": 1, "config": {"statusCode": 429, "retryAfter": "60s"}}]},
    {"pathPattern": "^/(orders|v2/orders)/", "probability": 1,
     "faults": [{"type": "error", "probability": 0.5, "config": {"defaultCode": 503}}]}
  ]}'

./mockd chaos enable --latency 800ms-2500ms     # slow Wolt, every endpoint
./mockd chaos disable
```

To exercise Wolt's webhook retries, point `follow --webhook-url` at an endpoint that answers non-200; the simulator retries `--retries` times, `--retry-delay` seconds apart, and logs each attempt.

## Tests

```bash
go build -o mockd ./cmd/mockd
MOCKD_BIN=$PWD/mockd python3 examples/wolt/tests/test_wolt_twin.py
```

23 end-to-end scenarios are defined, including five Increment B catalogue tests (effects,
unmatched identifiers, atomic rejection, venue isolation and concurrent price/stock updates).
The existing 18 scenarios cover OAuth, orders, webhooks, menus, venues and Drive. CI runs this
suite in the `wolt-twin` job. The five new scenarios await execution in the Go-enabled WSL workspace.

## Where the contract comes from

Wolt renders its API reference (developer.wolt.com/docs/api/*) from OpenAPI but does not publish the files. `specs/*.openapi.json` are generated by `tools/build_specs.py` from a capture of those pages (2026-06-29) published in [Nirbel/wolt-mcp](https://github.com/Nirbel/wolt-mcp) (MIT): operations, parameters, request/response schemas and Wolt's example payloads. The 38 operations of the Order, Menu, Venue, Time slot and Drive references are all served; anything not bound to state answers with Wolt's example or a schema-generated body.

```bash
git clone --depth 1 https://github.com/Nirbel/wolt-mcp /tmp/wolt-mcp
python3 -I examples/wolt/tools/build_specs.py /tmp/wolt-mcp/specs/wolt-official.json examples/wolt/specs
```

| Source | Used for |
|---|---|
| Official API reference (via the capture) | Paths, methods, success codes (202/204/201), request/response fields, enums, Drive error codes and envelope, the 25-minute `adjusted_pickup_time` limit, "reject only before accept", self-delivery-only endpoints, "location only after ready", one basket refund per request, async menu export |
| Wolt guides and changelog (developer.wolt.com, read via search) | Webhook envelope and `WOLT-SIGNATURE` HMAC, retry policy, notification statuses, venue alert types, OAuth lifetimes and single-use refresh tokens, Drive JWT webhooks and event names, exactly-one identifier and non-null inventory rules, Drive cancel path |
| Twin policy (no Wolt source) | Marketplace error body `{"detail"}`, `VENUE_NOT_FOUND` / `CANCELLATION_NOT_ALLOWED` codes, promise validity (10 min), delivery-area box, prices/ETAs, refund and replace-items state windows, Drive delivery statuses after `INFO_RECEIVED`, Drive webhook claim layout |

### Known differences from Wolt

- One host serves all APIs; Wolt splits them across `integrations-authentication-service`, `pos-integration-service` and `daas-public-api`.
- Only the Menu item and inventory updates validate bearer tokens against the OAuth provider; other endpoints accept any token except `expired-token`.
- Venue binding at `/oauth2/authorize?venue_id=` and the 401/403 split on guarded endpoints are twin policy (see `PRD-price-stock-pilot.md` §5).
- The menu export is `READY` immediately (Wolt may return `PENDING` first). Supported price/stock fields apply synchronously before 202; this is an approved simulation policy. `replace-items` still does not change order items.
- V1 and V2 fixtures share identity, status and delivery data, but their line items are Wolt's two different reference examples.
- Rate limits, venue offline alerts during opening hours and rejection alerts are not computed; trigger them with `chaos` and `venue-event`.
- Wolt's Order Submitter example contains a `"delivery:"` key (sic); the twin sends `delivery`.

## Files

| Path | Purpose |
|---|---|
| `mockd.yaml` | The twin: OAuth, imports, tables, business rules, bindings |
| `specs/*.openapi.json` | Generated Wolt contracts (do not edit by hand) |
| `specs/order-submitter.example.json` | Wolt's Order Submitter example payload |
| `tools/build_specs.py` | Snapshot → OpenAPI 3.0 converter |
| `sim/wolt_sim.py` | Webhooks, Order Submitter, venue alerts, Drive JWTs, Wolt-side actions, audit |
| `tests/test_wolt_twin.py` | End-to-end suite |
