from unittest.mock import MagicMock, patch

import frappe
from frappe.tests import IntegrationTestCase

RELAY_KEY = "test-relay-key"


def relay_response(status_code=200, body=None):
	response = MagicMock()
	response.status_code = status_code
	response.ok = status_code < 400
	response.json.return_value = body or {}
	return response


class TestWhatsAppAccount(IntegrationTestCase):
	def setUp(self):
		settings = frappe.get_single("WhatsApp Settings")
		settings.relay_base_url = "http://relay.test"
		settings.relay_api_key = RELAY_KEY
		settings.save(ignore_permissions=True)
		self.account = frappe.get_doc(
			{
				"doctype": "WhatsApp Account",
				"account_name": "Controls Test",
				"phone_number": "+1 555 000 1111",
				"status": "Active",
				"proxy": "socks5://proxy.test:1080",
			}
		).insert(ignore_permissions=True)

	def tearDown(self):
		frappe.db.rollback()

	def test_connect_sends_proxy(self):
		with patch("requests.post", return_value=relay_response(body={"status": "starting"})) as post:
			result = self.account.connect()
		self.assertEqual(result, {"status": "starting"})
		args, kwargs = post.call_args
		self.assertEqual(args[0], f"http://relay.test/session/{self.account.session_id}/start")
		self.assertEqual(kwargs["json"], {"proxy": "socks5://proxy.test:1080"})
		self.assertEqual(kwargs["headers"], {"x-api-key": RELAY_KEY})

	def test_connect_sends_null_proxy_when_unset(self):
		self.account.proxy = ""
		with patch("requests.post", return_value=relay_response()) as post:
			self.account.connect()
		self.assertEqual(post.call_args.kwargs["json"], {"proxy": None})

	def test_pair_with_code_requires_phone(self):
		self.account.phone_number = ""
		with patch("requests.post") as post:
			self.assertRaises(frappe.ValidationError, self.account.pair_with_code)
		post.assert_not_called()

	def test_pair_with_code_returns_code(self):
		with patch("requests.post", return_value=relay_response(body={"code": "ABCD-1234"})) as post:
			result = self.account.pair_with_code()
		self.assertEqual(result, {"code": "ABCD-1234"})
		self.assertEqual(post.call_args.kwargs["json"], {"phone": "+1 555 000 1111"})

	def test_pair_with_code_surfaces_relay_error(self):
		body = {"error": "already_paired", "detail": "Session is already paired"}
		with patch("requests.post", return_value=relay_response(409, body)):
			with self.assertRaises(frappe.ValidationError) as ctx:
				self.account.pair_with_code()
		self.assertIn("Session is already paired", str(ctx.exception))

	def test_pause_sets_paused(self):
		with patch("requests.post", return_value=relay_response()) as post:
			self.account.pause()
		self.assertTrue(post.call_args.args[0].endswith("/pause"))
		self.assertEqual(frappe.db.get_value("WhatsApp Account", self.account.name, "status"), "Paused")

	def test_pause_failure_leaves_status(self):
		with patch("requests.post", return_value=relay_response(500, {"error": "boom"})):
			self.assertRaises(frappe.ValidationError, self.account.pause)
		self.assertEqual(frappe.db.get_value("WhatsApp Account", self.account.name, "status"), "Active")

	def test_resume_sets_disconnected_and_sends_proxy(self):
		self.account.db_set("status", "Paused")
		with patch("requests.post", return_value=relay_response()) as post:
			self.account.resume()
		self.assertTrue(post.call_args.args[0].endswith("/resume"))
		self.assertEqual(post.call_args.kwargs["json"], {"proxy": "socks5://proxy.test:1080"})
		self.assertEqual(frappe.db.get_value("WhatsApp Account", self.account.name, "status"), "Disconnected")

	def test_logout_sets_disconnected(self):
		self.account.db_set("connected_at", frappe.utils.now_datetime())
		with patch("requests.post", return_value=relay_response()) as post:
			self.account.logout()
		self.assertTrue(post.call_args.args[0].endswith("/logout"))
		values = frappe.db.get_value(
			"WhatsApp Account", self.account.name, ["status", "connected_at"], as_dict=True
		)
		self.assertEqual(values.status, "Disconnected")
		self.assertIsNone(values.connected_at)

	def test_non_admin_forbidden(self):
		with patch("frappe.get_roles", return_value=["Guest"]), patch("requests.post") as post:
			for method in (self.account.connect, self.account.pair_with_code, self.account.pause,
				self.account.resume, self.account.logout):
				self.assertRaises(frappe.PermissionError, method)
		post.assert_not_called()
