import json
from unittest.mock import patch

import frappe
import requests
from frappe.database import get_db
from frappe.tests import IntegrationTestCase

from yourwhatsapp.yourwhatsapp import api
from yourwhatsapp.yourwhatsapp.doctype.whatsapp_settings import whatsapp_settings as ws

BASE_URL = "http://relay.test"
OLD_KEY = "old-key-" + "o" * 32
ROLE_USER = "relay-settings-admin@example.com"
METHODS = ("connect_relay", "rotate_key", "update_callback", "test_connection", "retry_pending")


def _response(status, body=None):
	response = requests.Response()
	response.status_code = status
	response._content = json.dumps(body if body is not None else {}).encode()
	return response


def _refused(status, code):
	return _response(status, {"error": code, "detail": code})


class FakeRelay:
	"""Models the Go relay's claim routes (relay/internal/api/api.go) in place of requests.request.

	The gate runs before every protected handler: 503 while unclaimed, then
	401 unless x-api-key is the current key (or the rotation grace key, where
	allowed). /info adds the keyed detail only for an accepted key. The grace
	key lives in memory only: expire_grace() stands for 10 minutes or a restart.

	`fail` answers a route without touching state (a 500, a refused connection);
	`lose` applies the change, then drops the answer (a lost 200).
	"""

	def __init__(self, mode="unclaimed", current=None, bootstrap_token=None, fail=None, lose=()):
		self.mode = mode
		self.current = current
		self.grace = None
		self.bootstrap_token = bootstrap_token
		self.fail = dict(fail or {})
		self.lose = set(lose)
		self.callback = None
		self.calls = []

	def __call__(self, method, url, headers=None, json=None, timeout=None):
		route = (method, url.removeprefix(BASE_URL))
		headers, body = headers or {}, json or {}
		self.calls.append({"route": route, "headers": headers, "json": json})
		if route in self.fail:
			answer = self.fail[route]
			answer = answer(headers, body) if callable(answer) else answer
			if isinstance(answer, Exception):
				raise answer
			return answer
		response = self._handle(route, headers.get("x-api-key"), body)
		if route in self.lose:
			raise requests.exceptions.ConnectionError("connection reset")
		return response

	def expire_grace(self):
		self.grace = None

	def _handle(self, route, key, body):
		if route == ("GET", "/info"):
			return self._info(key)
		if route == ("POST", "/claim"):
			return self._claim(body)
		if self.mode == "unclaimed":
			return _response(503, {"error": "unclaimed"})
		if route == ("POST", "/claim/rotate"):
			return self._rotate(key, body["api_key"])
		if route == ("PUT", "/claim/callback"):
			return self._set_callback(key, body)
		raise AssertionError(f"unexpected route {route}")

	def _info(self, key):
		body = {"version": "dev", "claimed": self.mode != "unclaimed", "mode": self.mode}
		if self._authorized(key):
			body.update(
				sessions={"connected": 1, "total": 2},
				outbox={"pending": 0, "dead": 0},
				callback_url=self.callback,
				site_name="s",
			)
		return _response(200, body)

	def _claim(self, body):
		if self.mode != "unclaimed":
			return _refused(409, "already_claimed")
		if self.bootstrap_token and body.get("bootstrap_token") != self.bootstrap_token:
			return _refused(401, "bad_bootstrap_token")
		if len(body.get("api_key") or "") < 32:
			return _refused(400, "bad_request")
		self.mode, self.current, self.callback = "claimed", body["api_key"], body["callback_url"]
		return _response(200, {"claimed": True})

	def _rotate(self, key, new_key):
		if not self._authorized(key):
			return _response(401, {"error": "invalid api key"})
		if key != self.current:
			# The grace key may repeat a rotation that already took effect.
			if new_key == self.current:
				return _response(200, {"ok": True})
			return _response(401, {"error": "invalid api key"})
		if self.mode == "env":
			return _refused(409, "env_mode")
		self.grace, self.current = self.current, new_key
		return _response(200, {"ok": True})

	def _set_callback(self, key, body):
		if not key or key != self.current:
			return _response(401, {"error": "invalid api key"})
		if self.mode == "env":
			return _refused(409, "env_mode")
		self.callback = body["callback_url"]
		return _response(200, {"ok": True})

	def _authorized(self, key):
		return bool(key) and key in (self.current, self.grace)

	def call(self, method, path):
		return next(c for c in self.calls if c["route"] == (method, path))

	def routes(self):
		return [c["route"] for c in self.calls]


class TestWhatsAppSettingsRelay(IntegrationTestCase):
	def setUp(self):
		settings = frappe.get_single("WhatsApp Settings")
		settings.relay_base_url = BASE_URL
		settings.relay_reachable_frappe_url = ""
		settings.relay_bootstrap_token = ""
		settings.relay_pending_key = ""
		settings.relay_api_key = ""
		settings.save(ignore_permissions=True)
		# The controller commits the pending key before calling the relay; in
		# tests that commit must not reach the live site.
		commit = patch.object(frappe.db, "commit")
		self.commit = commit.start()
		self.addCleanup(commit.stop)

	def tearDown(self):
		frappe.set_user("Administrator")
		frappe.db.rollback()
		# Writes above cached test values in Redis; the rollback doesn't undo those.
		frappe.clear_document_cache("WhatsApp Settings", "WhatsApp Settings")

	def _use(self, relay):
		request = patch.object(ws.requests, "request", side_effect=relay)
		request.start()
		self.addCleanup(request.stop)
		return relay

	def _settings(self):
		return frappe.get_single("WhatsApp Settings")

	def _keys(self):
		settings = self._settings()
		return (
			settings.get_password("relay_api_key", raise_exception=False),
			settings.get_password("relay_pending_key", raise_exception=False),
		)

	def _set_keys(self, current=None, pending=None):
		ws.write_password("relay_api_key", current)
		ws.write_password("relay_pending_key", pending)

	# Connect

	def test_connect_saves_key_only_on_success(self):
		relay = FakeRelay()
		seen = {}

		def claim(headers, body):
			# By the time the relay sees the key it is committed as pending,
			# and not yet the current key.
			seen["keys"], seen["committed"] = self._keys(), self.commit.called
			return relay._claim(body)

		relay.fail[("POST", "/claim")] = claim
		self._use(relay)

		result = self._settings().connect_relay()

		self.assertTrue(result["ok"])
		self.assertGreaterEqual(len(relay.current), 32)
		self.assertEqual(seen, {"keys": (None, relay.current), "committed": True})
		self.assertEqual(self._keys(), (relay.current, None))

	def test_connect_pending_survives_failure(self):
		self._set_keys(current=OLD_KEY)
		relay = self._use(FakeRelay(fail={("POST", "/claim"): _response(500, {"error": "internal"})}))

		result = self._settings().connect_relay()

		self.assertFalse(result["ok"])
		self.assertEqual(self._keys(), (OLD_KEY, relay.call("POST", "/claim")["json"]["api_key"]))

	def test_connect_env_mode_message(self):
		relay = self._use(FakeRelay(mode="env", current=OLD_KEY))

		result = self._settings().connect_relay()

		self.assertEqual(result["mode"], "env")
		self.assertIn("RELAY_API_KEY", result["message"])
		self.assertEqual(relay.routes(), [("GET", "/info")])
		self.assertEqual(self._keys(), (None, None))

	def test_connect_already_claimed_message(self):
		relay = self._use(FakeRelay(mode="claimed", current="someone-elses-key-" + "x" * 32))

		result = self._settings().connect_relay()

		self.assertFalse(result["ok"])
		self.assertEqual(result["message"], ws.ALREADY_CLAIMED)
		# Known from /info: no key generated, nothing sent, nothing pending.
		self.assertEqual(relay.routes(), [("GET", "/info")])
		self.assertEqual(self._keys(), (None, None))

	def test_connect_already_claimed_race_clears_pending(self):
		# Claimed by someone else between our /info and our /claim.
		relay = FakeRelay()
		relay.fail[("POST", "/claim")] = _refused(409, "already_claimed")
		self._use(relay)

		result = self._settings().connect_relay()

		self.assertEqual(result["message"], ws.ALREADY_CLAIMED)
		self.assertEqual(self._keys(), (None, None))

	def test_connect_sends_bootstrap_and_site(self):
		settings = self._settings()
		settings.relay_bootstrap_token = "boot-token"
		settings.save(ignore_permissions=True)
		relay = self._use(FakeRelay(bootstrap_token="boot-token"))

		self.assertTrue(self._settings().connect_relay()["ok"])

		body = relay.call("POST", "/claim")["json"]
		self.assertEqual(body["bootstrap_token"], "boot-token")
		self.assertEqual(body["site_name"], frappe.local.site)
		self.assertEqual(body["callback_url"], frappe.utils.get_url())
		self.assertNotIn("x-api-key", relay.call("GET", "/info")["headers"])

	def test_connect_bad_bootstrap_token_clears_pending(self):
		self._use(FakeRelay(bootstrap_token="the-right-token"))

		result = self._settings().connect_relay()

		self.assertFalse(result["ok"])
		self.assertIn("bootstrap token", result["message"])
		self.assertEqual(self._keys(), (None, None))

	def test_connect_uses_configured_callback_url(self):
		settings = self._settings()
		settings.relay_reachable_frappe_url = "http://frontend:8080"
		settings.save(ignore_permissions=True)
		relay = self._use(FakeRelay())

		self._settings().connect_relay()

		body = relay.call("POST", "/claim")["json"]
		self.assertEqual(body["callback_url"], "http://frontend:8080")
		self.assertNotIn("bootstrap_token", body)

	def test_connect_refused_while_key_pending(self):
		self._set_keys(current=OLD_KEY, pending="p" * 43)
		relay = self._use(FakeRelay())

		with self.assertRaises(frappe.ValidationError):
			self._settings().connect_relay()
		with self.assertRaises(frappe.ValidationError):
			self._settings().rotate_key()
		self.assertNotIn(("POST", "/claim"), relay.routes())
		self.assertNotIn(("POST", "/claim/rotate"), relay.routes())

	def test_key_change_refused_while_lock_held(self):
		self._set_keys(current=OLD_KEY)
		relay = self._use(FakeRelay(mode="claimed", current=OLD_KEY))
		other = self._second_connection()
		other.sql("SELECT GET_LOCK(%s, 0)", (ws.key_lock_name(),))
		try:
			with patch.object(ws, "KEY_LOCK_TIMEOUT", 0):
				with self.assertRaises(frappe.ValidationError):
					self._settings().rotate_key()
		finally:
			other.sql("SELECT RELEASE_LOCK(%s)", (ws.key_lock_name(),))
			other.close()
		self.assertEqual(relay.routes(), [])
		self.assertEqual(self._keys(), (OLD_KEY, None))

	def _second_connection(self):
		conf = frappe.conf
		db = get_db(
			socket=conf.db_socket,
			host=conf.db_host,
			port=conf.db_port,
			user=conf.db_user or conf.db_name,
			password=conf.db_password,
			cur_db_name=conf.db_name,
		)
		db.connect()
		return db

	# Rotate

	def test_rotate_uses_old_key_and_swaps_on_200(self):
		self._set_keys(current=OLD_KEY)
		relay = self._use(FakeRelay(mode="claimed", current=OLD_KEY))

		result = self._settings().rotate_key()

		call = relay.call("POST", "/claim/rotate")
		self.assertTrue(result["ok"])
		self.assertEqual(call["headers"]["x-api-key"], OLD_KEY)
		self.assertNotEqual(relay.current, OLD_KEY)
		self.assertEqual(self._keys(), (relay.current, None))

	def test_rotate_failure_keeps_old(self):
		self._set_keys(current=OLD_KEY)
		relay = self._use(
			FakeRelay(
				mode="claimed",
				current=OLD_KEY,
				fail={("POST", "/claim/rotate"): requests.exceptions.ConnectionError("refused")},
			)
		)

		result = self._settings().rotate_key()

		# Outcome unknown: keep the new key pending for Retry.
		self.assertFalse(result["ok"])
		self.assertEqual(self._keys(), (OLD_KEY, relay.call("POST", "/claim/rotate")["json"]["api_key"]))

	def test_rotate_rejected_keeps_old_and_clears_pending(self):
		self._set_keys(current=OLD_KEY)
		self._use(FakeRelay(mode="claimed", current="another-key-" + "a" * 32))

		result = self._settings().rotate_key()

		self.assertFalse(result["ok"])
		self.assertEqual(self._keys(), (OLD_KEY, None))

	def test_rotate_needs_a_current_key(self):
		self._use(FakeRelay(mode="claimed", current=OLD_KEY))
		with self.assertRaises(frappe.ValidationError):
			self._settings().rotate_key()

	def test_refusal_clears_pending_only_when_pending_not_accepted(self):
		# The relay refuses this rotate, but it already took the pending key
		# (an earlier attempt landed): promote it instead of throwing it away.
		self._set_keys(current=OLD_KEY)
		relay = FakeRelay(mode="claimed", current=OLD_KEY)

		def took_key_then_refused(headers, body):
			relay.current = body["api_key"]
			return _response(401, {"error": "invalid api key"})

		relay.fail[("POST", "/claim/rotate")] = took_key_then_refused
		self._use(relay)

		result = self._settings().rotate_key()

		self.assertTrue(result["ok"])
		self.assertEqual(self._keys(), (relay.current, None))
		info_keys = [c["headers"].get("x-api-key") for c in relay.calls if c["route"] == ("GET", "/info")]
		self.assertEqual(info_keys, [relay.current])

	# Retry

	def test_retry_after_lost_claim_200_no_current_key_promotes(self):
		relay = self._use(FakeRelay(lose={("POST", "/claim")}))
		self.assertFalse(self._settings().connect_relay()["ok"])
		pending = relay.current
		self.assertEqual(self._keys(), (None, pending))

		result = self._settings().retry_pending()

		self.assertTrue(result["ok"])
		self.assertEqual(self._keys(), (pending, None))
		self.assertEqual(relay.calls[-1]["route"], ("GET", "/info"))

	def test_retry_after_lost_rotate_200_after_grace_promotes(self):
		self._set_keys(current=OLD_KEY)
		relay = self._use(FakeRelay(mode="claimed", current=OLD_KEY, lose={("POST", "/claim/rotate")}))
		self.assertFalse(self._settings().rotate_key()["ok"])
		pending = relay.current
		relay.expire_grace()

		result = self._settings().retry_pending()

		self.assertTrue(result["ok"])
		self.assertEqual(self._keys(), (pending, None))
		self.assertEqual(relay.routes().count(("POST", "/claim/rotate")), 1)

	def test_retry_rotate_when_relay_never_got_it(self):
		self._set_keys(current=OLD_KEY, pending="n" * 43)
		relay = self._use(FakeRelay(mode="claimed", current=OLD_KEY))

		result = self._settings().retry_pending()

		call = relay.call("POST", "/claim/rotate")
		self.assertTrue(result["ok"])
		self.assertEqual((call["headers"]["x-api-key"], call["json"]["api_key"]), (OLD_KEY, "n" * 43))
		self.assertEqual(self._keys(), ("n" * 43, None))

	def test_retry_claim_on_unclaimed_relay(self):
		self._set_keys(pending="c" * 43)
		relay = self._use(FakeRelay())

		result = self._settings().retry_pending()

		self.assertTrue(result["ok"])
		self.assertEqual(relay.current, "c" * 43)
		self.assertEqual(self._keys(), ("c" * 43, None))

	def test_retry_both_keys_rejected_keeps_pending(self):
		self._set_keys(current=OLD_KEY, pending="n" * 43)
		relay = self._use(FakeRelay(mode="claimed", current="another-key-" + "a" * 32))

		result = self._settings().retry_pending()

		self.assertFalse(result["ok"])
		self.assertEqual(result["state"], "keys_rejected")
		self.assertIn("volume", result["message"])
		self.assertNotIn(("POST", "/claim/rotate"), relay.routes())
		self.assertEqual(self._keys(), (OLD_KEY, "n" * 43))

	def test_retry_unreachable_keeps_pending(self):
		self._set_keys(current=OLD_KEY, pending="n" * 43)
		self._use(FakeRelay(fail={("GET", "/info"): requests.exceptions.ConnectionError("refused")}))

		self.assertFalse(self._settings().retry_pending()["ok"])
		self.assertEqual(self._keys(), (OLD_KEY, "n" * 43))

	def test_retry_without_pending_key(self):
		self._use(FakeRelay())
		with self.assertRaises(frappe.ValidationError):
			self._settings().retry_pending()

	# Callback, test connection, verify

	def test_update_callback(self):
		self._set_keys(current=OLD_KEY)
		settings = self._settings()
		settings.relay_reachable_frappe_url = "http://frontend:8080"
		settings.save(ignore_permissions=True)
		relay = self._use(FakeRelay(mode="claimed", current=OLD_KEY))

		result = self._settings().update_callback()

		call = relay.call("PUT", "/claim/callback")
		self.assertTrue(result["ok"])
		self.assertEqual(call["headers"]["x-api-key"], OLD_KEY)
		self.assertEqual(
			call["json"], {"callback_url": "http://frontend:8080", "site_name": frappe.local.site}
		)

	def test_update_callback_env_mode(self):
		self._set_keys(current=OLD_KEY)
		self._use(FakeRelay(mode="env", current=OLD_KEY))

		result = self._settings().update_callback()

		self.assertFalse(result["ok"])
		self.assertIn("FRAPPE_BASE_URL", result["message"])

	def test_test_connection_states(self):
		self._set_keys(current=OLD_KEY)
		cases = {
			"unreachable": FakeRelay(fail={("GET", "/info"): requests.exceptions.ConnectionError("refused")}),
			"key_rejected": FakeRelay(fail={("GET", "/info"): _response(401, {"error": "invalid api key"})}),
			"unclaimed": FakeRelay(),
			"ok": FakeRelay(mode="claimed", current=OLD_KEY),
		}
		for state, relay in cases.items():
			with self.subTest(state=state), patch.object(ws.requests, "request", side_effect=relay):
				result = self._settings().test_connection()
				self.assertEqual(result["state"], state)
				self.assertEqual(relay.calls[0]["headers"]["x-api-key"], OLD_KEY)
				self.assertFalse(result["pending"])
		self.assertEqual(result["sessions"], {"connected": 1, "total": 2})

	def test_test_connection_claimed_without_detail_is_key_rejected(self):
		# /info answers 200 to a wrong key, just without the keyed detail.
		self._set_keys(current=OLD_KEY)
		self._use(FakeRelay(mode="claimed", current="another-key-" + "a" * 32))
		self.assertEqual(self._settings().test_connection()["state"], "key_rejected")

	def test_verify_accepts_pending_key(self):
		pending = "v" * 43
		self._set_keys(current=OLD_KEY, pending=pending)

		for key in (OLD_KEY, pending):
			with patch("frappe.get_request_header", return_value=key):
				api._verify_relay_key()
		for key in ("wrong", "", None):
			with patch("frappe.get_request_header", return_value=key):
				self.assertRaises(frappe.PermissionError, api._verify_relay_key)

	def test_non_system_manager_forbidden(self):
		self._make_whatsapp_admin()
		relay = self._use(FakeRelay())
		frappe.set_user(ROLE_USER)

		for method in METHODS:
			with self.subTest(method=method), self.assertRaises(frappe.PermissionError):
				getattr(self._settings(), method)()
		self.assertEqual(relay.calls, [])

	def _make_whatsapp_admin(self):
		if frappe.db.exists("User", ROLE_USER):
			return
		user = frappe.get_doc(
			{"doctype": "User", "email": ROLE_USER, "first_name": "Relay", "send_welcome_email": 0}
		).insert(ignore_permissions=True)
		user.add_roles("WhatsApp Admin")

	def test_key_never_in_error_message(self):
		pending = "q" * 43

		def echo_error(headers, body):
			message = f"boom {headers.get('x-api-key')} {body.get('api_key')}"
			return requests.exceptions.ConnectionError(message)

		def echo_500(headers, body):
			return _response(500, {"error": f"{headers.get('x-api-key')}", "detail": body.get("api_key")})

		cases = [
			("rotate_key", ("POST", "/claim/rotate"), "claimed", None),
			("connect_relay", ("POST", "/claim"), "unclaimed", None),
			("retry_pending", ("POST", "/claim/rotate"), "claimed", pending),
			("test_connection", ("GET", "/info"), "claimed", pending),
		]
		for method, route, mode, pending_key in cases:
			for answer in (echo_error, echo_500):
				with self.subTest(method=method, answer=answer.__name__):
					self._set_keys(current=OLD_KEY, pending=pending_key)
					frappe.local.message_log = []
					relay = FakeRelay(mode=mode, current=OLD_KEY, fail={route: answer})
					with patch.object(ws.requests, "request", side_effect=relay):
						result = getattr(self._settings(), method)()
					sent = {c["json"]["api_key"] for c in relay.calls if c["json"] and "api_key" in c["json"]}
					shown = json.dumps(result) + json.dumps(frappe.local.message_log)
					self.assertTrue(result.get("ok") is False or result.get("state") == "unreachable")
					for key in {OLD_KEY, pending} | sent:
						self.assertNotIn(key, shown)
