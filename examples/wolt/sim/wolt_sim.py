#!/usr/bin/env python3
"""Wolt-side simulator for the mockd Wolt digital twin.

mockd answers the calls a partner makes *to* Wolt. This script plays the
other direction: it creates orders the way Wolt does, and delivers the
signed callbacks Wolt sends *to* the partner.

  new-order    Create an order in the twin and (via `follow`) notify the partner.
  wolt         Run a Wolt-side transition: cancel, courier, deliver, preorder-production.
  follow       Turn the twin's event log into webhooks. With --auto, also play
               Wolt's own behaviour (couriers, auto-reject, pre-order start, Drive).
  venue-event  Send a venue alert webhook (offline, rejections, opening hours).
  audit        Check captured requests for missing `Authorization: Bearer`.

Standard library only (Python 3.9+). Run `wolt_sim.py <command> -h` for options.
Configuration also comes from environment variables (flags win):

  WOLT_MOCK_URL         twin base URL                          http://localhost:4280
  WOLT_ADMIN_URL        mockd admin API                        http://localhost:4290
  WOLT_ADMIN_KEY        admin API key, if the admin runs with auth
  WOLT_WEBHOOK_URL      partner endpoint for order/venue webhooks
  WOLT_WEBHOOK_SECRET   client secret used for WOLT-SIGNATURE   wolt-pilot-secret
  WOLT_DRIVE_WEBHOOK_URL / WOLT_DRIVE_WEBHOOK_SECRET   Drive callback + JWT secret
  WOLT_ORDER_MODE       "notification" (default) or "submitter" (full order push)
  WOLT_SIM_STATE        where `follow` keeps its event cursor   <tmp>/wolt_sim_state.json
"""

import argparse
import base64
import copy
import hashlib
import hmac
import json
import os
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timedelta, timezone

DEFAULT_VENUE = "68656c6c6f76326f72646572"

# Partner/Wolt action recorded by the twin -> Wolt order notification status.
# Statuses marked optional are only sent with --optional-events (Wolt enables
# them per integration).
ORDER_WEBHOOKS = {
    "created": "CREATED",
    "accept": "PRODUCTION",
    "self_delivery_accept": "PRODUCTION",
    "wolt_preorder_production": "PRODUCTION",
    "ready": "READY",
    "reject": "CANCELED",
    "wolt_cancel": "CANCELED",
    "delivered": "DELIVERED",
    "wolt_delivered": "DELIVERED",
}
OPTIONAL_ORDER_WEBHOOKS = {
    "wolt_courier_courier_at_venue": "COURIER ARRIVAL",
    "wolt_courier_picked_up": "PICK-UP-COMPLETED",
    "pickup_completed": "PICK-UP-COMPLETED",
}
# Keys of the Order Submitter payload (a subset of the v1 order).
SUBMITTER_KEYS = [
    "id", "venue", "price", "delivery", "items", "created_at", "consumer_comment",
    "pickup_eta", "attribution_id", "type", "pre_order", "consumer_name",
    "consumer_phone_number", "order_number", "order_status", "modified_at",
    "company_tax_id", "loyalty_card_number",
]
VENUE_EVENTS = [
    "REJECTION_ALERT_TRIGGERED", "REJECTION_ALERT_RECOVERED",
    "VENUE_OFFLINE_ALERT_TRIGGERED", "VENUE_OFFLINE_ALERT_RECOVERED",
    "OPENING_HOURS_UPDATED",
]
DRIVE_FLOW = [  # (event type, delivery status written to the twin)
    ("order.received", None),
    ("order.pickup_started", "PICKUP_STARTED"),
    ("order.pickup_arrival", None),
    ("order.picked_up", "PICKED_UP"),
    ("order.dropoff_started", None),
    ("order.dropoff_arrival", None),
    ("order.dropoff_completed", None),
    ("order.delivered", "DELIVERED"),
]
STATE_FILE = os.environ.get("WOLT_SIM_STATE", os.path.join(tempfile.gettempdir(), "wolt_sim_state.json"))


# ── helpers ─────────────────────────────────────────────────────────────────


def now_iso(delta_minutes=0):
    t = datetime.now(timezone.utc) + timedelta(minutes=delta_minutes)
    return t.strftime("%Y-%m-%dT%H:%M:%S.") + f"{t.microsecond // 1000:03d}Z"


def log(msg):
    print(f"[{datetime.now().strftime('%H:%M:%S')}] {msg}", flush=True)


class Twin:
    """Thin client for the mockd admin API."""

    def __init__(self, admin_url, mock_url, admin_key=None):
        self.admin = admin_url.rstrip("/")
        self.mock = mock_url.rstrip("/")
        self.key = admin_key

    def _call(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.admin + path, data=data, method=method)
        req.add_header("Content-Type", "application/json")
        if self.key:
            req.add_header("X-API-Key", self.key)
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                raw = resp.read()
                return resp.status, (json.loads(raw) if raw else None)
        except urllib.error.HTTPError as err:
            raw = err.read()
            try:
                return err.code, json.loads(raw)
            except ValueError:
                return err.code, raw.decode(errors="replace")

    def get(self, table, item_id):
        status, body = self._call("GET", f"/state/resources/{table}/items/{item_id}")
        return body if status == 200 else None

    def create(self, table, item):
        status, body = self._call("POST", f"/state/resources/{table}/items", item)
        if status not in (200, 201):
            raise SystemExit(f"create {table} failed: {status} {body}")
        return body

    def events(self, table, after):
        status, body = self._call("GET", f"/state/resources/{table}/items?limit=1000&sort=createdAt&order=asc")
        if status != 200:
            raise SystemExit(f"cannot read {table}: {status} {body} (is the twin running?)")
        items = sorted(body.get("data", []), key=lambda e: int(e["id"]))
        if not items or int(items[-1]["id"]) < after:
            after = 0  # the twin was reset: sequence ids start again at 1
        return [e for e in items if int(e["id"]) > after]

    def run(self, operation, payload):
        return self._call("POST", f"/state/operations/{operation}/execute", payload)

    def requests(self, limit=1000):
        status, body = self._call("GET", f"/requests?limit={limit}")
        if status != 200:
            raise SystemExit(f"cannot read request log: {status} {body}")
        return body.get("requests") or body.get("data") or []


def strip_internal(doc):
    return {k: v for k, v in doc.items() if k not in ("createdAt", "updatedAt")}


def sign(secret, raw):
    return hmac.new(secret.encode(), raw, hashlib.sha256).hexdigest()


def jwt_hs256(secret, claims):
    def b64(data):
        return base64.urlsafe_b64encode(data).rstrip(b"=").decode()

    header = b64(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
    payload = b64(json.dumps(claims, separators=(",", ":")).encode())
    sig = hmac.new(secret.encode(), f"{header}.{payload}".encode(), hashlib.sha256).digest()
    return f"{header}.{payload}.{b64(sig)}"


def post(url, body, headers, retries, retry_delay, label):
    """POST like Wolt: retry until the partner answers 200 (default 3 retries, 5 s apart)."""
    raw = json.dumps(body, separators=(",", ":")).encode()
    for attempt in range(retries + 1):
        req = urllib.request.Request(url, data=raw, method="POST")
        req.add_header("Content-Type", "application/json")
        for k, v in headers(raw).items():
            req.add_header(k, v)
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                code = resp.status
        except urllib.error.HTTPError as err:
            code = err.code
        except (urllib.error.URLError, OSError) as err:
            code = f"unreachable ({err})"
        log(f"→ {label} → {url} [{code}]" + (f" attempt {attempt + 1}" if attempt else ""))
        if code == 200:
            return True
        if attempt < retries:
            time.sleep(retry_delay)
    log(f"✗ {label}: gave up after {retries + 1} attempts")
    return False


# ── order factory ───────────────────────────────────────────────────────────


def build_order(twin, kind, venue_id, order_id):
    """Clone a seed order from the twin and turn it into a fresh order of `kind`."""
    template_id = {
        "delivery": "6710a1000000000000000001",
        "takeaway": "6710a1000000000000000002",
        "preorder": "6710a1000000000000000003",
        "self-delivery": "6710a1000000000000000004",
    }[kind]
    docs = {}
    for table in ("orders_v2", "orders"):
        seed = twin.get(table, template_id)
        if seed is None:
            raise SystemExit(f"seed order {template_id} missing in {table}; reset the twin (POST /state/reset)")
        doc = strip_internal(copy.deepcopy(seed))
        doc["id"] = order_id
        doc["order_number"] = str(int(time.time()) % 1000)
        doc["order_status"] = "received"
        doc["created_at"] = doc["modified_at"] = now_iso()
        doc["pickup_eta"] = now_iso(20)
        doc["venue"]["id"] = venue_id
        if doc.get("delivery"):
            doc["delivery"]["time"] = now_iso(45 if kind != "takeaway" else 20)
        if kind == "preorder":
            doc["pre_order"] = {"preorder_time": now_iso(180), "pre_order_status": "waiting"}
            doc["pickup_eta"] = now_iso(160)
        docs[table] = doc
    return docs


# ── commands ────────────────────────────────────────────────────────────────


def cmd_new_order(args, twin):
    order_id = args.id or uuid.uuid4().hex[:24]
    docs = build_order(twin, args.kind, args.venue, order_id)
    twin.create("orders_v2", docs["orders_v2"])
    twin.create("orders", docs["orders"])
    twin.create("order_events", {
        "order_id": order_id, "venue_id": args.venue, "action": "created",
        "order_status": "received", "at": now_iso(), "payload": {"kind": args.kind},
    })
    log(f"created {args.kind} order {order_id} (venue {args.venue})")
    print(order_id)


def cmd_wolt(args, twin):
    ops = {
        "cancel": ("WoltCancelOrder", {}),
        "courier-arrived": ("WoltCourierUpdate", {"status": "courier_at_venue"}),
        "picked-up": ("WoltCourierUpdate", {"status": "picked_up"}),
        "courier-at-customer": ("WoltCourierUpdate", {"status": "courier_at_delivery_location"}),
        "deliver": ("WoltDeliverOrder", {}),
        "preorder-production": ("WoltStartPreorderProduction", {}),
    }
    op, extra = ops[args.action]
    status, body = twin.run(op, {"orderId": args.order_id, **extra})
    log(f"{op} {args.order_id}: {status} {body if status != 200 else ''}".rstrip())
    if status != 200:
        sys.exit(1)


class Follower:
    def __init__(self, args, twin):
        self.args, self.twin = args, twin
        self.state = self._load_state()
        self.pending = []  # (due_epoch, description, callable)

    def _load_state(self):
        if self.args.from_start or not os.path.exists(STATE_FILE):
            return {"order_events": 0, "venue_events": 0, "drive_events": 0}
        with open(STATE_FILE) as fh:
            return json.load(fh)

    def _save_state(self):
        with open(STATE_FILE, "w") as fh:
            json.dump(self.state, fh)

    def schedule(self, seconds, description, fn):
        self.pending.append((time.time() + seconds, description, fn))

    # webhooks
    def order_webhook(self, order_id, venue_id, status):
        a = self.args
        if not a.webhook_url:
            log(f"(no --webhook-url) would send {status} for {order_id}")
            return
        if a.order_mode == "submitter":
            order = self.twin.get("orders", order_id)
            body = {k: order.get(k) for k in SUBMITTER_KEYS} if order else {"id": order_id}
            label = f"order submitter {status} {order_id}"
        else:
            body = {
                "id": str(uuid.uuid4()),
                "type": "order.notification",
                "order": {
                    "id": order_id,
                    "venue_id": venue_id,
                    "status": status,
                    "resource_url": f"{self.twin.mock}/orders/{order_id}",
                },
                "created_at": now_iso(),
            }
            label = f"order.notification {status} {order_id}"
        post(a.webhook_url, body, lambda raw: {"WOLT-SIGNATURE": sign(a.webhook_secret, raw)},
             a.retries, a.retry_delay, label)

    def venue_webhook(self, venue_id, external_venue_id, event_type, details=None):
        a = self.args
        if not a.webhook_url:
            log(f"(no --webhook-url) would send {event_type} for venue {venue_id}")
            return
        body = {
            "event_type": event_type,
            "venue_id": venue_id,
            "external_venue_id": external_venue_id,
            "detected_at": now_iso(),
            "details": details or {},
        }
        post(a.webhook_url, body, lambda raw: {"WOLT-SIGNATURE": sign(a.webhook_secret, raw)},
             a.retries, a.retry_delay, f"venue {event_type}")

    def drive_webhook(self, delivery_id, event_type):
        a = self.args
        d = self.twin.get("deliveries", delivery_id)
        if d is None:
            return
        if not a.drive_webhook_url:
            log(f"(no --drive-webhook-url) would send {event_type} for {delivery_id}")
            return
        claims = {
            "id": str(uuid.uuid4()),
            "type": event_type,
            "dispatched_at": now_iso(),
            "venue_id": d.get("venue_id"),
            "wolt_order_reference_id": d.get("wolt_order_reference_id"),
            "tracking_reference": (d.get("tracking") or {}).get("id"),
            "merchant_order_reference_id": d.get("merchant_order_reference_id"),
            "order_number": d.get("order_number"),
            "price": d.get("price"),
            "pickup": {"eta": (d.get("pickup") or {}).get("eta")},
            "dropoff": {"eta": (d.get("dropoff") or {}).get("eta")},
            "courier": None if event_type == "order.received" else {"name": "Mock Courier", "location": None},
            "parcels": d.get("parcels"),
        }
        body = {"token": jwt_hs256(a.drive_webhook_secret, claims)}
        post(a.drive_webhook_url, body, lambda raw: {}, a.retries, a.retry_delay, f"drive {event_type} {delivery_id}")

    # Wolt behaviour (--auto)
    def run_op(self, op, payload):
        status, body = self.twin.run(op, payload)
        if status != 200:
            log(f"  {op} skipped: {status} {body}")

    def auto_order(self, ev):
        a = self.args
        order_id, action = ev["order_id"], ev["action"]
        if action == "created" and a.auto_reject_after > 0:
            def maybe_reject():
                o = self.twin.get("orders_v2", order_id)
                if o and o["order_status"] in ("received", "fetched", "acknowledged") and o.get("type") != "preorder":
                    log(f"auto-reject: {order_id} not accepted in {a.auto_reject_after}s")
                    self.run_op("WoltCancelOrder", {"orderId": order_id, "reason": "not_accepted_in_time"})
            self.schedule(a.auto_reject_after, f"auto-reject check {order_id}", maybe_reject)
        if action == "confirm_preorder":
            self.schedule(a.preorder_delay, f"pre-order production {order_id}",
                          lambda: self.run_op("WoltStartPreorderProduction", {"orderId": order_id}))
        if action in ("accept", "wolt_preorder_production"):
            o = self.twin.get("orders_v2", order_id)
            if o and o["delivery"]["type"] == "homedelivery" and not o["delivery"].get("self_delivery"):
                self.schedule(a.courier_delay, f"courier at venue {order_id}",
                              lambda: self.run_op("WoltCourierUpdate", {"orderId": order_id, "status": "courier_at_venue"}))
        if action == "ready":
            o = self.twin.get("orders_v2", order_id)
            if o and o["delivery"]["type"] == "homedelivery" and not o["delivery"].get("self_delivery"):
                self.schedule(a.courier_delay, f"courier picked up {order_id}",
                              lambda: self.run_op("WoltCourierUpdate", {"orderId": order_id, "status": "picked_up"}))
                self.schedule(a.courier_delay * 2, f"delivered {order_id}",
                              lambda: self.run_op("WoltDeliverOrder", {"orderId": order_id}))

    def auto_drive(self, ev):
        if ev["kind"] != "delivery_created":
            return
        ref = ev["ref"]
        for i, (event_type, status) in enumerate(DRIVE_FLOW[1:], start=1):
            def step(event_type=event_type, status=status):
                d = self.twin.get("deliveries", ref)
                if not d or d.get("status") == "CANCELLED":
                    return
                if status:
                    self.run_op("WoltDriveUpdate", {"woltOrderReferenceId": ref, "status": status})
                self.drive_webhook(ref, event_type)
            self.schedule(self.args.courier_delay * i, f"drive {event_type} {ref}", step)

    def tick(self):
        """Process new events and due Wolt actions. Returns how much work was done."""
        a = self.args
        seen = 0
        for ev in self.twin.events("order_events", self.state["order_events"]):
            seen += 1
            self.state["order_events"] = int(ev["id"])
            status = ORDER_WEBHOOKS.get(ev["action"])
            if status is None and a.optional_events:
                status = OPTIONAL_ORDER_WEBHOOKS.get(ev["action"])
            log(f"order {ev['order_id']}: {ev['action']}")
            if status:
                self.order_webhook(ev["order_id"], ev["venue_id"], status)
            if a.auto:
                self.auto_order(ev)
        for ev in self.twin.events("venue_events", self.state["venue_events"]):
            seen += 1
            self.state["venue_events"] = int(ev["id"])
            log(f"venue {ev['venue_id']}: {ev['action']}")
            if ev["action"] in ("opening_times", "special_opening_times"):
                self.venue_webhook(ev["venue_id"], ev.get("external_venue_id"), "OPENING_HOURS_UPDATED")
        for ev in self.twin.events("drive_events", self.state["drive_events"]):
            seen += 1
            self.state["drive_events"] = int(ev["id"])
            log(f"drive {ev['ref']}: {ev['kind']}")
            if ev["kind"] == "delivery_created":
                self.drive_webhook(ev["ref"], "order.received")
            if a.auto:
                self.auto_drive(ev)
        due = [p for p in self.pending if p[0] <= time.time()]
        self.pending = [p for p in self.pending if p[0] > time.time()]
        for _, description, fn in sorted(due, key=lambda p: p[0]):
            log(f"wolt: {description}")
            fn()
        self._save_state()
        return seen + len(due)  # actions just run may have written new events

    def run(self):
        log(f"following {self.twin.admin} (webhooks → {self.args.webhook_url or 'none'}, "
            f"drive → {self.args.drive_webhook_url or 'none'}, auto={self.args.auto})")
        while True:
            seen = self.tick()
            # --once drains: stop only when nothing new arrived and nothing is scheduled.
            if self.args.once and not self.pending and seen == 0:
                return
            time.sleep(self.args.interval)


def cmd_follow(args, twin):
    try:
        Follower(args, twin).run()
    except KeyboardInterrupt:
        pass


def cmd_venue_event(args, twin):
    v = twin.get("venues", args.venue)
    external = (v or {}).get("external_venue_id")
    Follower(args, twin).venue_webhook(args.venue, external, args.event_type, json.loads(args.details or "{}"))


def cmd_audit(args, twin):
    prefixes = ("/orders/", "/v2/orders/", "/v1/", "/venues/", "/merchants/", "/order/")
    missing = []
    for r in twin.requests():
        path = r.get("path") or ""
        if not path.startswith(prefixes):
            continue
        headers = {k.lower(): v for k, v in (r.get("headers") or {}).items()}
        auth = headers.get("authorization")
        auth = auth[0] if isinstance(auth, list) and auth else auth
        if not (isinstance(auth, str) and auth.startswith("Bearer ")):
            missing.append(f"{r.get('method')} {path}")
    if missing:
        print("Requests without 'Authorization: Bearer <token>':")
        for m in missing:
            print("  " + m)
        sys.exit(1)
    print("OK: every captured Wolt API request carried a bearer token")


# ── CLI ─────────────────────────────────────────────────────────────────────


def main(argv=None):
    env = os.environ.get
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--mock-url", default=env("WOLT_MOCK_URL", "http://localhost:4280"))
    p.add_argument("--admin-url", default=env("WOLT_ADMIN_URL", "http://localhost:4290"))
    p.add_argument("--admin-key", default=env("WOLT_ADMIN_KEY"))
    sub = p.add_subparsers(dest="command", required=True)

    def webhook_opts(sp):
        sp.add_argument("--webhook-url", default=env("WOLT_WEBHOOK_URL"))
        sp.add_argument("--webhook-secret", default=env("WOLT_WEBHOOK_SECRET", "wolt-pilot-secret"))
        sp.add_argument("--drive-webhook-url", default=env("WOLT_DRIVE_WEBHOOK_URL"))
        sp.add_argument("--drive-webhook-secret", default=env("WOLT_DRIVE_WEBHOOK_SECRET", "wolt-drive-secret"))
        sp.add_argument("--order-mode", choices=["notification", "submitter"],
                        default=env("WOLT_ORDER_MODE", "notification"))
        sp.add_argument("--retries", type=int, default=3, help="retries until the partner answers 200 (Wolt: 3)")
        sp.add_argument("--retry-delay", type=float, default=5.0, help="seconds between retries (Wolt: 5)")

    sp = sub.add_parser("new-order", help="create an order as Wolt would")
    sp.add_argument("--kind", choices=["delivery", "takeaway", "preorder", "self-delivery"], default="delivery")
    sp.add_argument("--venue", default=DEFAULT_VENUE)
    sp.add_argument("--id", help="order id (default: random 24-hex)")
    sp.set_defaults(fn=cmd_new_order)

    sp = sub.add_parser("wolt", help="run a Wolt-side transition on an order")
    sp.add_argument("action", choices=["cancel", "courier-arrived", "picked-up", "courier-at-customer",
                                       "deliver", "preorder-production"])
    sp.add_argument("order_id")
    sp.set_defaults(fn=cmd_wolt)

    sp = sub.add_parser("follow", help="deliver webhooks for the twin's event log")
    webhook_opts(sp)
    sp.add_argument("--auto", action="store_true", help="also simulate couriers, auto-reject, pre-orders, Drive")
    sp.add_argument("--optional-events", action="store_true",
                    help="also send COURIER ARRIVAL / PICK-UP-COMPLETED notifications")
    sp.add_argument("--auto-reject-after", type=float, default=0,
                    help="cancel orders not accepted within N seconds (0 = off)")
    sp.add_argument("--courier-delay", type=float, default=5.0, help="seconds between simulated courier steps")
    sp.add_argument("--preorder-delay", type=float, default=10.0,
                    help="seconds from pre-order confirmation to production")
    sp.add_argument("--interval", type=float, default=1.0, help="poll interval in seconds")
    sp.add_argument("--from-start", action="store_true", help="replay the whole event log")
    sp.add_argument("--once", action="store_true", help="process pending events and exit")
    sp.set_defaults(fn=cmd_follow)

    sp = sub.add_parser("venue-event", help="send a venue alert webhook")
    webhook_opts(sp)
    sp.add_argument("event_type", choices=VENUE_EVENTS)
    sp.add_argument("--venue", default=DEFAULT_VENUE)
    sp.add_argument("--details", help="JSON object for the details field")
    sp.set_defaults(fn=cmd_venue_event)

    sp = sub.add_parser("audit", help="check captured requests for a bearer token")
    sp.set_defaults(fn=cmd_audit)

    args = p.parse_args(argv)
    twin = Twin(args.admin_url, args.mock_url, args.admin_key)
    args.fn(args, twin)


if __name__ == "__main__":
    main()
