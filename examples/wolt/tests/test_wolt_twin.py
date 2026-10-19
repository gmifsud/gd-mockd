#!/usr/bin/env python3
"""End-to-end tests for the Wolt digital twin.

Boots `mockd` with examples/wolt/mockd.yaml, runs a webhook receiver that
verifies WOLT-SIGNATURE (HMAC-SHA256) and Drive JWTs (HS256), and drives
every flow through the HTTP API and sim/wolt_sim.py.

  python3 examples/wolt/tests/test_wolt_twin.py            # uses `mockd` on PATH
  MOCKD_BIN=./mockd python3 examples/wolt/tests/test_wolt_twin.py
  WOLT_TWIN_EXTERNAL=1 ...   # reuse an already running twin on 4280/4290 (state is reset)
"""

import base64
import hashlib
import hmac
import http.server
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, os.path.join(ROOT, "sim"))
import wolt_sim  # noqa: E402

MOCK = "http://localhost:4280"
ADMIN = "http://localhost:4290"
SECRET = "wolt-pilot-secret"
DRIVE_SECRET = "wolt-drive-secret"
VENUE = "68656c6c6f76326f72646572"
DRIVE_VENUE = "6710d0000000000000000001"
BEARER = {"Authorization": "Bearer test-token"}


def call(method, path, body=None, headers=None, base=MOCK):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    for k, v in {**BEARER, **(headers or {})}.items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as err:
        raw = err.read()
        return err.code, (json.loads(raw) if raw else None)


def form(path, fields, user, password):
    req = urllib.request.Request(MOCK + path, data=urllib.parse.urlencode(fields).encode(), method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    req.add_header("Authorization", "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode())
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as err:
        return err.code, json.loads(err.read())


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def verify_jwt(token, secret):
    header, payload, sig = token.split(".")
    expected = hmac.new(secret.encode(), f"{header}.{payload}".encode(), hashlib.sha256).digest()
    actual = base64.urlsafe_b64decode(sig + "=" * (-len(sig) % 4))
    if not hmac.compare_digest(expected, actual):
        raise AssertionError("bad JWT signature")
    return json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))


class Receiver(http.server.BaseHTTPRequestHandler):
    """Partner webhook endpoint: /wolt (marketplace) and /drive (Wolt Drive)."""

    received = []
    lock = threading.Lock()

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        body = json.loads(raw)
        if self.path == "/wolt":
            expected = hmac.new(SECRET.encode(), raw, hashlib.sha256).hexdigest()
            ok = hmac.compare_digest(expected, self.headers.get("WOLT-SIGNATURE", ""))
            entry = {"kind": "wolt", "valid": ok, "body": body}
        else:
            try:
                entry = {"kind": "drive", "valid": True, "body": verify_jwt(body["token"], DRIVE_SECRET)}
            except Exception:  # noqa: BLE001
                entry = {"kind": "drive", "valid": False, "body": body}
        with self.lock:
            self.received.append(entry)
        self.send_response(200)
        self.end_headers()

    def log_message(self, *args):
        pass


class WoltTwinTest(unittest.TestCase):
    proc = None

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="wolt-twin-")
        if not os.environ.get("WOLT_TWIN_EXTERNAL"):
            mockd = os.environ.get("MOCKD_BIN") or shutil.which("mockd")
            if not mockd:
                raise unittest.SkipTest("mockd not found (set MOCKD_BIN)")
            env = {**os.environ, "HOME": cls.tmp}
            cls.proc = subprocess.Popen(
                [mockd, "start", "-c", os.path.join(ROOT, "mockd.yaml"), "--no-auth",
                 "--data-dir", os.path.join(cls.tmp, "data")],
                stdout=open(os.path.join(cls.tmp, "mockd.log"), "w"), stderr=subprocess.STDOUT, env=env,
            )
        # Ready once the config is imported and a seeded order is served.
        for _ in range(150):
            try:
                if call("GET", "/v2/orders/6710a1000000000000000001")[0] == 200:
                    break
            except Exception:  # noqa: BLE001
                pass
            time.sleep(0.2)
        else:
            raise RuntimeError("twin did not start; see " + os.path.join(cls.tmp, "mockd.log"))
        cls.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.hook = f"http://127.0.0.1:{cls.server.server_address[1]}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        if cls.proc:
            cls.proc.terminate()
            cls.proc.wait(timeout=10)
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def setUp(self):
        call("POST", "/state/reset", base=ADMIN)
        call("DELETE", "/requests", base=ADMIN)
        with Receiver.lock:
            Receiver.received.clear()
        self.state_file = os.path.join(self.tmp, "sim-state.json")
        wolt_sim.STATE_FILE = self.state_file
        if os.path.exists(self.state_file):
            os.remove(self.state_file)

    # helpers
    def sim(self, *argv):
        wolt_sim.main(["--mock-url", MOCK, "--admin-url", ADMIN, *argv])

    def follow(self, *extra):
        self.sim("follow", "--once", "--interval", "0.05", "--retries", "0",
                 "--webhook-url", self.hook + "/wolt", "--drive-webhook-url", self.hook + "/drive",
                 "--courier-delay", "0.1", "--preorder-delay", "0.1", *extra)

    def notifications(self, order_id):
        with Receiver.lock:
            entries = [e for e in Receiver.received if e["kind"] == "wolt"]
        for e in entries:
            self.assertTrue(e["valid"], "WOLT-SIGNATURE must verify")
        return [e["body"]["order"]["status"] for e in entries
                if "order" in e["body"] and e["body"]["order"]["id"] == order_id]

    def new_order(self, kind):
        order_id = os.urandom(12).hex()
        self.sim("new-order", "--kind", kind, "--id", order_id)
        return order_id

    # ── Authentication 2.0 ──
    def test_oauth_authorization_code_and_single_use_refresh(self):
        redirect = "http://localhost:3000/wolt/oauth/callback"
        opener = urllib.request.build_opener(NoRedirect)
        query = urllib.parse.urlencode({"client_id": "wolt-pilot-client", "redirect_uri": redirect,
                                        "response_type": "code", "state": "s1", "scope": "offline"})
        with self.assertRaises(urllib.error.HTTPError) as ctx:
            opener.open(f"{MOCK}/oauth2/authorize?{query}")
        location = urllib.parse.urlparse(ctx.exception.headers["Location"])
        params = urllib.parse.parse_qs(location.query)
        self.assertEqual(params["state"], ["s1"])

        status, tokens = form("/oauth2/token", {"grant_type": "authorization_code", "code": params["code"][0],
                                                "redirect_uri": redirect}, "wolt-pilot-client", "wolt-pilot-secret")
        self.assertEqual(status, 200)
        self.assertEqual(tokens["expires_in"], 3600)
        status, _ = form("/oauth2/token", {"grant_type": "authorization_code", "code": params["code"][0],
                                           "redirect_uri": redirect}, "wolt-pilot-client", "wolt-pilot-secret")
        self.assertEqual(status, 400, "authorization codes are single-use")

        status, refreshed = form("/oauth2/token", {"grant_type": "refresh_token", "refresh_token": tokens["refresh_token"]},
                                 "wolt-pilot-client", "wolt-pilot-secret")
        self.assertEqual(status, 200)
        self.assertNotEqual(refreshed["refresh_token"], tokens["refresh_token"])
        status, err = form("/oauth2/token", {"grant_type": "refresh_token", "refresh_token": tokens["refresh_token"]},
                           "wolt-pilot-client", "wolt-pilot-secret")
        self.assertEqual((status, err["error"]), (400, "invalid_grant"), "refresh tokens are single-use")

    def test_expired_token_is_rejected(self):
        status, body = call("GET", "/v2/orders/6710a1000000000000000001", headers={"Authorization": "Bearer expired-token"})
        self.assertEqual((status, body), (401, {"detail": "Unauthorized"}))

    # ── Order API ──
    def test_wolt_delivered_order_full_lifecycle(self):
        oid = self.new_order("delivery")
        self.follow()
        self.assertEqual(self.notifications(oid), ["CREATED"])
        resource = call("GET", f"/orders/{oid}")
        self.assertEqual(resource[0], 200)
        self.assertEqual(resource[1]["order_status"], "received")

        self.assertEqual(call("PUT", f"/orders/{oid}/ready")[0], 409, "ready before accept")
        self.assertEqual(call("PUT", f"/orders/{oid}/accept", {})[0], 202)
        self.assertEqual(call("PUT", f"/orders/{oid}/accept", {})[0], 409, "double accept")
        self.assertEqual(call("PUT", f"/orders/{oid}/reject", {"reason": "late"})[0], 409, "reject after accept")
        self.assertEqual(call("PUT", f"/orders/{oid}/ready")[0], 202)
        self.follow("--auto")

        self.assertEqual(self.notifications(oid), ["CREATED", "PRODUCTION", "READY", "DELIVERED"])
        _, v2 = call("GET", f"/v2/orders/{oid}")
        _, v1 = call("GET", f"/orders/{oid}")
        self.assertEqual((v2["order_status"], v2["delivery"]["status"]), ("delivered", "delivered"))
        self.assertEqual((v1["order_status"], v1["delivery"]["status"]), ("delivered", "delivered"))
        self.assertIn("basket_price", v2)
        self.assertIn("price", v1)

    def test_accept_validates_adjusted_pickup_time(self):
        oid = self.new_order("delivery")
        _, order = call("GET", f"/v2/orders/{oid}")
        eta = order["pickup_eta"]
        too_late = wolt_sim.datetime.strptime(eta, "%Y-%m-%dT%H:%M:%S.%fZ") + wolt_sim.timedelta(minutes=26)
        status, body = call("PUT", f"/orders/{oid}/accept", {"adjusted_pickup_time": too_late.strftime("%Y-%m-%dT%H:%M:%SZ")})
        self.assertEqual(status, 400)
        self.assertIn("25 minutes", body["detail"])
        ok = wolt_sim.datetime.strptime(eta, "%Y-%m-%dT%H:%M:%S.%fZ") + wolt_sim.timedelta(minutes=10)
        adjusted = ok.strftime("%Y-%m-%dT%H:%M:%SZ")
        self.assertEqual(call("PUT", f"/orders/{oid}/accept", {"adjusted_pickup_time": adjusted})[0], 202)
        self.assertEqual(call("GET", f"/v2/orders/{oid}")[1]["pickup_eta"], adjusted)

    def test_takeaway_reject_and_refund_rules(self):
        oid = self.new_order("takeaway")
        self.assertEqual(call("PUT", f"/orders/{oid}/reject", {})[0], 400, "reason is required")
        self.assertEqual(call("PUT", f"/orders/{oid}/reject", {"reason": "x", "code": "NOPE"})[0], 400)
        self.assertEqual(call("POST", f"/orders/{oid}/refund-items",
                              {"reason": "QUALITY_ISSUES", "item_refunds": [{"id": "i"}]})[0], 409)
        self.assertEqual(call("PUT", f"/orders/{oid}/reject", {"reason": "Out of stock", "code": "ITEMS_UNAVAILABLE"})[0], 202)
        self.follow()
        self.assertEqual(self.notifications(oid), ["CREATED", "CANCELED"])
        self.assertEqual(call("GET", f"/orders/{oid}")[1]["order_status"], "rejected")

        done = "6710a1000000000000000005"
        self.assertEqual(call("POST", f"/orders/{done}/refund-basket", {"basket_refunds": [{}, {}]})[0], 400)
        self.assertEqual(call("POST", f"/orders/{done}/refund-basket", {"basket_refunds": [{"amount": 100}]})[0], 202)
        self.assertEqual(call("PUT", f"/orders/{done}/sent-to-pos",
                              {"success": True, "time_sent_to_pos": "2026-10-19T10:00:00Z"})[0], 204)

    def test_takeaway_is_completed_by_the_partner(self):
        oid = self.new_order("takeaway")
        for step in ("accept", "ready", "delivered"):
            self.assertEqual(call("PUT", f"/orders/{oid}/{step}", {})[0], 202, step)
        self.follow("--auto")
        self.assertEqual(self.notifications(oid), ["CREATED", "PRODUCTION", "READY", "DELIVERED"])
        self.assertEqual(call("GET", f"/v2/orders/{oid}")[1]["delivery"]["type"], "takeaway")

    def test_self_delivery_flow(self):
        oid = self.new_order("self-delivery")
        self.assertEqual(call("PUT", f"/orders/{oid}/accept", {})[0], 409, "self-delivery uses its own accept")
        self.assertEqual(call("PUT", f"/orders/{oid}/self-delivery/accept", {"total_delivery_time": "2026-10-19T11:00:00Z"})[0], 202)
        self.assertEqual(call("PUT", f"/orders/{oid}/delivery/tracking/location", {"location": {"lat": 60.1, "lon": 24.9}})[0], 400,
                         "location only after ready")
        self.assertEqual(call("PUT", f"/orders/{oid}/ready")[0], 202)
        self.assertEqual(call("PUT", f"/orders/{oid}/delivery/tracking/location", {"location": {"lat": 60.1, "lon": 24.9}})[0], 202)
        self.assertEqual(call("PUT", f"/orders/{oid}/delivery/eta", {"eta_minutes": 12})[0], 202)
        self.assertEqual(call("PUT", f"/orders/{oid}/pickup-completed")[0], 202)
        self.assertEqual(call("GET", f"/v2/orders/{oid}")[1]["delivery"]["status"], "picked_up")
        self.assertEqual(call("PUT", f"/orders/{oid}/courier-at-customer")[0], 202)
        self.assertEqual(call("PUT", f"/orders/{oid}/delivered")[0], 202)
        self.follow("--auto")
        self.assertEqual(self.notifications(oid), ["CREATED", "PRODUCTION", "READY", "DELIVERED"])

        normal = self.new_order("delivery")
        self.assertEqual(call("PUT", f"/orders/{normal}/delivery/eta", {"eta_minutes": 5})[0], 409)

    def test_preorder_confirmation_and_production(self):
        oid = self.new_order("preorder")
        self.assertEqual(call("PUT", f"/orders/{oid}/confirm-preorder")[0], 204)
        self.assertEqual(call("PUT", f"/orders/{oid}/confirm-preorder")[0], 409)
        self.follow("--auto")
        _, order = call("GET", f"/v2/orders/{oid}")
        self.assertEqual(order["pre_order"]["pre_order_status"], "confirmed")
        self.assertEqual(order["order_status"], "production")
        self.assertEqual(self.notifications(oid), ["CREATED", "PRODUCTION"])

    def test_auto_reject_and_wolt_cancel(self):
        late = self.new_order("delivery")
        self.follow("--auto", "--auto-reject-after", "0.1")
        self.assertEqual(self.notifications(late), ["CREATED", "CANCELED"])
        self.assertEqual(call("PUT", f"/orders/{late}/accept", {})[0], 409)

        oid = self.new_order("delivery")
        call("PUT", f"/orders/{oid}/accept", {})
        self.sim("wolt", "cancel", oid)
        self.follow()
        self.assertEqual(self.notifications(oid), ["CREATED", "PRODUCTION", "CANCELED"])

    def test_order_submitter_mode_pushes_full_order(self):
        oid = self.new_order("delivery")
        self.follow("--order-mode", "submitter")
        with Receiver.lock:
            pushes = [e for e in Receiver.received if e["kind"] == "wolt" and e["body"].get("id") == oid]
        self.assertEqual(len(pushes), 1)
        self.assertTrue(pushes[0]["valid"])
        self.assertEqual(pushes[0]["body"]["order_status"], "received")
        self.assertIn("items", pushes[0]["body"])

    # ── Venue API ──
    def test_venue_status_online_and_opening_hours(self):
        self.assertEqual(call("PATCH", f"/venues/{VENUE}/online", {"status": "PAUSED"})[0], 400)
        self.assertEqual(call("PATCH", f"/venues/{VENUE}/online", {"status": "OFFLINE", "until": None})[0], 204)
        status, venue = call("GET", f"/venues/{VENUE}/status")
        self.assertEqual(status, 200)
        self.assertFalse(venue["status"]["is_online"])
        self.assertFalse([k for k in venue if k.startswith("_") or k == "id"], "internal fields stay hidden")
        self.assertEqual(call("GET", f"/venues/{VENUE}/settings"), (200, {"status": {"is_open": True}}))
        self.assertEqual(call("PATCH", f"/venues/{VENUE}/delivery-provider", {"delivery_provider": "SELF_DELIVERY"})[0], 204)
        self.assertEqual(call("GET", f"/venues/{VENUE}/delivery-provider"), (200, {"delivery_provider": "SELF_DELIVERY"}))
        hours = [{"opening_day": "MONDAY", "opening_time": "10:00", "closing_day": "MONDAY", "closing_time": "22:00"}]
        self.assertEqual(call("PATCH", f"/venues/{VENUE}/opening-times", {"availability": hours})[0], 202)
        self.assertEqual(call("GET", f"/venues/{VENUE}/status")[1]["opening_times"], hours)
        self.follow()
        with Receiver.lock:
            alerts = [e["body"]["event_type"] for e in Receiver.received if "event_type" in e["body"]]
        self.assertEqual(alerts, ["OPENING_HOURS_UPDATED"])
        self.assertEqual(call("GET", "/venues/unknown/status")[0], 404)

    # ── Menu API ──
    def test_menu_push_export_and_item_updates(self):
        menu = {"id": "m2", "currency": "EUR", "primary_language": "en",
                "categories": [{"id": "c1", "name": [{"lang": "en", "value": "Mains"}], "items": []}]}
        self.assertEqual(call("POST", f"/v1/restaurants/{VENUE}/menu", menu)[0], 202)
        status, req = call("GET", f"/v2/venues/{VENUE}/menu")
        self.assertEqual(status, 202)
        self.assertEqual(set(req), {"request_id", "resource_url"})
        with urllib.request.urlopen(req["resource_url"]) as resp:
            export = json.loads(resp.read())
        self.assertEqual(export["status"], "READY")
        self.assertEqual(export["request_id"], req["request_id"])
        self.assertEqual(export["menu"], menu)

        self.assertEqual(call("PATCH", f"/venues/{VENUE}/items", {"data": [{"external_id": "1", "sku": "1", "enabled": False}]})[0], 400)
        self.assertEqual(call("PATCH", f"/venues/{VENUE}/items", {"data": [{"external_id": "1", "enabled": False}]})[0], 202)
        self.assertEqual(call("PATCH", f"/venues/{VENUE}/items/inventory", {"data": [{"gtin": "1", "inventory": None}]})[0], 400)
        self.assertEqual(call("PATCH", f"/venues/{VENUE}/items/inventory", {"data": [{"gtin": "1", "inventory": 3}]})[0], 202)
        self.assertEqual(call("PATCH", f"/venues/{VENUE}/options/values", {"data": [{"external_id": "TRUF", "enabled": False}]})[0], 202)

    # ── Wolt Drive ──
    def test_drive_promise_delivery_webhooks_and_cancel(self):
        base = f"/v1/venues/{DRIVE_VENUE}"
        self.assertEqual(call("POST", "/v1/venues/unknown/shipment-promises", {"lat": 60.17, "lon": 24.93})[1]["error_code"],
                         "VENUE_NOT_FOUND")
        status, err = call("POST", f"{base}/shipment-promises", {"lat": 62.0, "lon": 24.9})
        self.assertEqual((status, err["error_code"]), (400, "DROPOFF_OUTSIDE_OF_DELIVERY_AREA"))
        self.assertFalse(call("POST", f"{base}/shipment-promises", {"street": "Mannerheimintie 1", "city": "Helsinki"})[1]["is_binding"])
        status, promise = call("POST", f"{base}/shipment-promises",
                               {"lat": 60.168, "lon": 24.94, "street": "Mannerheimintie 1", "city": "Helsinki",
                                "post_code": "00100", "min_preparation_time_minutes": 10})
        self.assertEqual(status, 201)
        self.assertTrue(promise["is_binding"])
        self.assertEqual(promise["time_estimate_minutes"], 35)

        delivery = {"shipment_promise_id": promise["id"], "merchant_order_reference_id": "POS-1",
                    "recipient": {"name": "Yuho", "phone_number": "+3581234567"},
                    "parcels": [{"count": 1, "description": "Bag"}],
                    "dropoff": {"location": {"coordinates": {"lat": 60.168, "lon": 24.94}}, "comment": "Door 1234"}}
        status, created = call("POST", f"{base}/deliveries", delivery)
        self.assertEqual(status, 201)
        self.assertEqual(created["status"], "INFO_RECEIVED")
        self.assertEqual(created["wolt_order_reference_id"], created["tracking"]["id"])
        self.assertEqual(call("POST", f"{base}/deliveries", delivery)[1]["error_code"], "DUPLICATE_ORDER")
        self.assertEqual(call("POST", f"{base}/deliveries", {**delivery, "shipment_promise_id": "nope",
                                                             "merchant_order_reference_id": "POS-2"})[1]["error_code"],
                         "SHIPMENT_PROMISE_NOT_FOUND")

        self.follow("--auto")
        with Receiver.lock:
            drive = [e for e in Receiver.received if e["kind"] == "drive"]
        self.assertTrue(all(e["valid"] for e in drive), "Drive JWTs must verify")
        self.assertEqual([e["body"]["type"] for e in drive], [t for t, _ in wolt_sim.DRIVE_FLOW])
        self.assertEqual(drive[0]["body"]["merchant_order_reference_id"], "POS-1")
        ref = created["wolt_order_reference_id"]
        status, err = call("PATCH", f"/order/{ref}/status/cancel")
        self.assertEqual((status, err["error_code"]), (409, "CANCELLATION_NOT_ALLOWED"))

        promise2 = call("POST", f"{base}/shipment-promises", {"lat": 60.168, "lon": 24.94})[1]
        fresh = call("POST", f"{base}/deliveries", {**delivery, "shipment_promise_id": promise2["id"],
                                                    "merchant_order_reference_id": "POS-3"})[1]
        self.assertEqual(call("PATCH", f"/order/{fresh['id']}/status/cancel")[0], 200)

    # ── Contract-only endpoints and audit ──
    def test_contract_only_endpoints_serve_wolt_examples(self):
        status, slots = call("GET", f"/v1/venues/{VENUE}/time-slots")
        self.assertEqual(status, 200)
        self.assertIn("effective_time_slots", slots)
        self.assertEqual(call("POST", "/orders/x/documents/invoice/upload-links")[0], 201)

    def test_audit_detects_missing_bearer(self):
        call("GET", "/v2/orders/6710a1000000000000000001")
        self.sim("audit")
        urllib.request.urlopen(MOCK + "/v2/orders/6710a1000000000000000001")  # no Authorization header
        with self.assertRaises(SystemExit):
            self.sim("audit")


if __name__ == "__main__":
    unittest.main(verbosity=2)
