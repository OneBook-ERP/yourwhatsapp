import json
from unittest.mock import patch

import frappe
import requests
from frappe.tests import IntegrationTestCase

from yourwhatsapp.yourwhatsapp import api
from yourwhatsapp.yourwhatsapp.doctype.whatsapp_settings import whatsapp_settings as ws

BASE_URL = "http://relay.test"
OLD_KEY = "old-key-" + "o" * 32
ROLE_USER = "relay-settings-admin@example.com"


def _response(status, body=None):
	response = requests.Response()
	response.status_code = status
	response._content = json.dumps(body if body is not None else {}).encode()
	return response


class FakeRelay:
	"""Stands in for requests.request: answers per (method, path), records every call."""

	def __init__(self, routes):
		self.routes = routes
		self.calls = []

	def __call__(self, method, url, headers=None, json=None, timeout=None):
		path = url.removeprefix(BASE_URL)
		self.calls.append({"method": method, "path": path, "headers": headers or {}, "json": json})
		answer = self.routes[(method, path)]
		if callable(answer):
			answer = answer(headers or {}, json)
		if isinstance(answer, Exception):
			raise answer
		return answer

	def call(self, method, path):
		return next(c for c in self.calls if (c["method"], c["path"]) == (method, path))

	def paths(self):
		return [(c["method"], c["path"]) for c in self.calls]


UNCLAIMED = _response(200, {"version": "dev", "claimed": False, "mode": "unclaimed"})
CLAIMED = _response(200, {"version": "dev", "claimed": True, "mode": "claimed"})
ENV = _response(200, {"version": "dev", "claimed": True, "mode": "env"})


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

	def _relay(self, routes):
		relay = FakeRelay(routes)
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

	def _set_current_key(self, key):
		ws.write_password("relay_api_key", key)

	def test_connect_saves_key_only_on_success(self):
		seen = {}

		def claim(headers, body):
			# By the time the relay sees the key it is committed as pending,
			# and not yet the current key.
			seen["keys"] = self._keys()
			seen["committed"] = self.commit.called
			return _response(200, {"claimed": True})

		self._relay({("GET", "/info"): UNCLAIMED, ("POST", "/claim"): claim})
		result = self._settings().connect_relay()

		sent = self._relay_key_sent()
		self.assertTrue(result["ok"])
		self.assertGreaterEqual(len(sent), 32)
		self.assertEqual(seen["keys"], (None, sent))
		self.assertTrue(seen["committed"])
		self.assertEqual(self._keys(), (sent, None))

	def _relay_key_sent(self):
		return ws.requests.request.call_args.kwargs["json"]["api_key"]

	def test_connect_pending_survives_failure(self):
		self._set_current_key(OLD_KEY)
		self._relay({("GET", "/info"): UNCLAIMED, ("POST", "/claim"): _response(500, {"error": "internal"})})

		result = self._settings().connect_relay()

		self.assertFalse(result["ok"])
		self.assertEqual(self._keys(), (OLD_KEY, self._relay_key_sent()))

	def test_connect_env_mode_message(self):
		relay = self._relay({("GET", "/info"): ENV})

		result = self._settings().connect_relay()

		self.assertEqual(result["mode"], "env")
		self.assertIn("RELAY_API_KEY", result["message"])
		self.assertEqual(relay.paths(), [("GET", "/info")])
		self.assertEqual(self._keys(), (None, None))

	def test_connect_already_claimed_message(self):
		self._relay(
			{("GET", "/info"): CLAIMED, ("POST", "/claim"): _response(409, {"error": "already_claimed"})}
		)

		result = self._settings().connect_relay()

		self.assertFalse(result["ok"])
		self.assertEqual(result["message"], ws.ALREADY_CLAIMED)
		# A refusal is definite: the relay never took the key, so nothing to retry.
		self.assertEqual(self._keys(), (None, None))

	def test_connect_sends_bootstrap_and_site(self):
		settings = self._settings()
		settings.relay_bootstrap_token = "boot-token"
		settings.save(ignore_permissions=True)
		relay = self._relay({("GET", "/info"): UNCLAIMED, ("POST", "/claim"): _response(200)})

		self._settings().connect_relay()

		body = relay.call("POST", "/claim")["json"]
		self.assertEqual(body["bootstrap_token"], "boot-token")
		self.assertEqual(body["site_name"], frappe.local.site)
		self.assertEqual(body["callback_url"], frappe.utils.get_url())
		self.assertNotIn("x-api-key", relay.call("GET", "/info")["headers"])

	def test_connect_uses_configured_callback_url(self):
		settings = self._settings()
		settings.relay_reachable_frappe_url = "http://frontend:8080"
		settings.save(ignore_permissions=True)
		relay = self._relay({("GET", "/info"): UNCLAIMED, ("POST", "/claim"): _response(200)})

		self._settings().connect_relay()

		body = relay.call("POST", "/claim")["json"]
		self.assertEqual(body["callback_url"], "http://frontend:8080")
		self.assertNotIn("bootstrap_token", body)

	def test_connect_refused_while_key_pending(self):
		ws.write_password("relay_pending_key", "p" * 43)
		relay = self._relay({})

		with self.assertRaises(frappe.ValidationError):
			self._settings().connect_relay()
		with self.assertRaises(frappe.ValidationError):
			self._settings().rotate_key()
		self.assertEqual(relay.calls, [])

	def test_rotate_uses_old_key_and_swaps_on_200(self):
		self._set_current_key(OLD_KEY)
		relay = self._relay({("POST", "/claim/rotate"): _response(200, {"ok": True})})

		result = self._settings().rotate_key()

		call = relay.call("POST", "/claim/rotate")
		new_key = call["json"]["api_key"]
		self.assertTrue(result["ok"])
		self.assertEqual(call["headers"]["x-api-key"], OLD_KEY)
		self.assertNotEqual(new_key, OLD_KEY)
		self.assertEqual(self._keys(), (new_key, None))

	def test_rotate_failure_keeps_old(self):
		self._set_current_key(OLD_KEY)
		self._relay({("POST", "/claim/rotate"): requests.exceptions.ConnectionError("refused")})

		result = self._settings().rotate_key()

		# Outcome unknown: keep the new key pending for Retry.
		self.assertFalse(result["ok"])
		self.assertEqual(self._keys(), (OLD_KEY, self._relay_key_sent()))

	def test_rotate_rejected_keeps_old_and_clears_pending(self):
		self._set_current_key(OLD_KEY)
		self._relay({("POST", "/claim/rotate"): _response(401, {"error": "invalid api key"})})

		result = self._settings().rotate_key()

		self.assertFalse(result["ok"])
		self.assertEqual(self._keys(), (OLD_KEY, None))

	def test_rotate_needs_a_current_key(self):
		self._relay({})
		with self.assertRaises(frappe.ValidationError):
			self._settings().rotate_key()

	def test_retry_rotate_after_lost_200(self):
		self._set_current_key(OLD_KEY)
		pending = "n" * 43
		ws.write_password("relay_pending_key", pending)
		relay = self._relay(
			{("GET", "/info"): CLAIMED, ("POST", "/claim/rotate"): _response(200, {"ok": True})}
		)

		result = self._settings().retry_pending()

		call = relay.call("POST", "/claim/rotate")
		self.assertTrue(result["ok"])
		self.assertEqual((call["headers"]["x-api-key"], call["json"]["api_key"]), (OLD_KEY, pending))
		self.assertEqual(self._keys(), (pending, None))

	def test_retry_claim_on_unclaimed_relay(self):
		pending = "c" * 43
		ws.write_password("relay_pending_key", pending)
		relay = self._relay({("GET", "/info"): UNCLAIMED, ("POST", "/claim"): _response(200)})

		result = self._settings().retry_pending()

		self.assertTrue(result["ok"])
		self.assertEqual(relay.call("POST", "/claim")["json"]["api_key"], pending)
		self.assertEqual(self._keys(), (pending, None))

	def test_retry_without_pending_key(self):
		self._relay({})
		with self.assertRaises(frappe.ValidationError):
			self._settings().retry_pending()

	def test_update_callback(self):
		self._set_current_key(OLD_KEY)
		settings = self._settings()
		settings.relay_reachable_frappe_url = "http://frontend:8080"
		settings.save(ignore_permissions=True)
		relay = self._relay({("PUT", "/claim/callback"): _response(200, {"ok": True})})

		result = self._settings().update_callback()

		call = relay.call("PUT", "/claim/callback")
		self.assertTrue(result["ok"])
		self.assertEqual(call["headers"]["x-api-key"], OLD_KEY)
		self.assertEqual(
			call["json"], {"callback_url": "http://frontend:8080", "site_name": frappe.local.site}
		)

	def test_update_callback_env_mode(self):
		self._set_current_key(OLD_KEY)
		self._relay({("PUT", "/claim/callback"): _response(409, {"error": "env_mode", "detail": "env"})})

		result = self._settings().update_callback()

		self.assertFalse(result["ok"])
		self.assertIn("FRAPPE_BASE_URL", result["message"])

	def test_test_connection_states(self):
		self._set_current_key(OLD_KEY)
		ok_body = {
			"version": "v1",
			"claimed": True,
			"mode": "claimed",
			"sessions": {"connected": 1, "total": 2},
			"outbox": {"pending": 0, "dead": 0},
			"callback_url": "http://frontend:8080",
			"site_name": "s",
		}
		cases = {
			"unreachable": requests.exceptions.ConnectionError("refused"),
			"key_rejected": _response(401, {"error": "invalid api key"}),
			"unclaimed": UNCLAIMED,
			"ok": _response(200, ok_body),
		}
		for state, answer in cases.items():
			with self.subTest(state=state):
				relay = FakeRelay({("GET", "/info"): answer})
				with patch.object(ws.requests, "request", side_effect=relay):
					result = self._settings().test_connection()
				self.assertEqual(result["state"], state)
				self.assertEqual(relay.calls[0]["headers"]["x-api-key"], OLD_KEY)
				self.assertFalse(result["pending"])
		self.assertEqual(result["sessions"], {"connected": 1, "total": 2})

	def test_test_connection_claimed_without_detail_is_key_rejected(self):
		# /info answers 200 to a wrong key, just without the keyed detail.
		self._set_current_key(OLD_KEY)
		self._relay({("GET", "/info"): CLAIMED})
		self.assertEqual(self._settings().test_connection()["state"], "key_rejected")

	def test_verify_accepts_pending_key(self):
		self._set_current_key(OLD_KEY)
		pending = "v" * 43
		ws.write_password("relay_pending_key", pending)

		for key in (OLD_KEY, pending):
			with patch("frappe.get_request_header", return_value=key):
				api._verify_relay_key()
		for key in ("wrong", "", None):
			with patch("frappe.get_request_header", return_value=key):
				self.assertRaises(frappe.PermissionError, api._verify_relay_key)

	def test_non_system_manager_forbidden(self):
		self._make_whatsapp_admin()
		relay = self._relay({})
		frappe.set_user(ROLE_USER)

		for method in ("connect_relay", "rotate_key", "update_callback", "test_connection", "retry_pending"):
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
		self._set_current_key(OLD_KEY)

		def echo(headers, body):
			return requests.exceptions.ConnectionError(f"boom {headers.get('x-api-key')} {body['api_key']}")

		def echo_500(headers, body):
			return _response(500, {"error": f"{headers.get('x-api-key')}", "detail": body["api_key"]})

		for answer in (echo, echo_500):
			with self.subTest(answer=answer.__name__):
				ws.write_password("relay_pending_key", None)
				frappe.local.message_log = []
				relay = FakeRelay({("POST", "/claim/rotate"): answer})
				with patch.object(ws.requests, "request", side_effect=relay):
					result = self._settings().rotate_key()
				new_key = relay.calls[0]["json"]["api_key"]
				shown = json.dumps(result) + json.dumps(frappe.local.message_log)
				self.assertFalse(result["ok"])
				self.assertNotIn(OLD_KEY, shown)
				self.assertNotIn(new_key, shown)
