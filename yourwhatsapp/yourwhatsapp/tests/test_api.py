from datetime import timedelta
from unittest.mock import patch

import frappe
from frappe.tests import IntegrationTestCase
from frappe.utils import add_to_date, now_datetime

from yourwhatsapp.yourwhatsapp import api

RELAY_KEY = "test-relay-key"
FAILOVER_TARGET = "yourwhatsapp.yourwhatsapp.relay_client.handle_async_failure"


class TestRelayCallbacks(IntegrationTestCase):
	def setUp(self):
		settings = frappe.get_single("WhatsApp Settings")
		settings.relay_api_key = RELAY_KEY
		settings.save(ignore_permissions=True)

		key_patch = patch("frappe.get_request_header", return_value=RELAY_KEY)
		key_patch.start()
		self.addCleanup(key_patch.stop)
		# Outgoing messages enqueue a send on insert; keep that, and the
		# failover dispatch, out of the real queue.
		self.enqueue = patch("frappe.enqueue")
		self.enqueue_mock = self.enqueue.start()
		self.addCleanup(self.enqueue.stop)

		self.account = self._make_account("Relay Test A")

	def tearDown(self):
		frappe.db.rollback()

	def _make_account(self, name, **fields):
		return frappe.get_doc(
			{"doctype": "WhatsApp Account", "account_name": name, "phone_number": "15550001111", **fields}
		).insert(ignore_permissions=True)

	def _make_outgoing(self, status="Queued", message_id=None):
		doc = frappe.get_doc(
			{
				"doctype": "WhatsApp Message",
				"type": "Outgoing",
				"whatsapp_account": self.account.name,
				"to": "15551234567",
				"content_type": "text",
				"message": "hi",
			}
		).insert(ignore_permissions=True)
		doc.db_set({"status": status, "message_id": message_id})
		return doc

	def _status(self, name):
		return frappe.db.get_value("WhatsApp Message", name, ["status", "message_id", "error_reason"], as_dict=True)

	def _failover_calls(self):
		return [c for c in self.enqueue_mock.call_args_list if c.args and c.args[0] == FAILOVER_TARGET]

	def _inbound(self, **kwargs):
		return api.inbound_message(
			session_id=self.account.session_id, from_number="15557654321", message="hello", **kwargs
		)

	def test_message_status_by_local_id_sets_message_id(self):
		msg = self._make_outgoing()
		api.message_status(status="Sent", message_id="WAID1", local_message_id=msg.name)
		row = self._status(msg.name)
		self.assertEqual((row.status, row.message_id), ("Sent", "WAID1"))

	def test_message_status_monotonic(self):
		msg = self._make_outgoing(status="Sent", message_id="WAID2")
		api.message_status(status="Read", message_id="WAID2")
		api.message_status(status="Delivered", message_id="WAID2")
		self.assertEqual(self._status(msg.name).status, "Read")

	def test_failed_ignored_after_sent(self):
		msg = self._make_outgoing(status="Sent", message_id="WAID3")
		api.message_status(status="Failed", message_id="WAID3", error_code="send_failed")
		self.assertEqual(self._status(msg.name).status, "Sent")
		self.assertEqual(self._failover_calls(), [])

	def test_failed_enqueues_failover(self):
		msg = self._make_outgoing()
		api.message_status(
			status="Failed",
			local_message_id=msg.name,
			error_code="send_failed",
			error_reason="boom",
		)
		row = self._status(msg.name)
		self.assertEqual((row.status, row.error_reason), ("Failed", "boom"))
		calls = self._failover_calls()
		self.assertEqual(len(calls), 1)
		self.assertEqual(calls[0].kwargs["message_name"], msg.name)
		self.assertEqual(calls[0].kwargs["error_code"], "send_failed")

	def test_temp_banned_failure_rate_limits_account(self):
		msg = self._make_outgoing()
		api.message_status(status="Failed", local_message_id=msg.name, error_code="temp_banned")
		self.assertEqual(frappe.db.get_value("WhatsApp Account", self.account.name, "status"), "RateLimited")
		self.assertEqual(len(self._failover_calls()), 1)

	def test_invalid_number_failed_no_failover(self):
		msg = self._make_outgoing()
		api.message_status(status="Failed", local_message_id=msg.name, error_code="invalid_number")
		self.assertEqual(self._status(msg.name).status, "Failed")
		self.assertEqual(self._failover_calls(), [])

	def test_unknown_id_noop(self):
		result = api.message_status(status="Delivered", message_id="NOPE")
		self.assertFalse(result["ok"])
		result = api.message_status(status="Failed", local_message_id="does-not-exist", error_code="send_failed")
		self.assertFalse(result["ok"])
		self.assertEqual(self._failover_calls(), [])

	def test_inbound_dedupe(self):
		first = self._inbound(message_id="IN1")
		second = self._inbound(message_id="IN1")
		self.assertEqual(first["name"], second["name"])
		self.assertEqual(frappe.db.count("WhatsApp Message", {"message_id": "IN1", "type": "Incoming"}), 1)

	def test_inbound_from_lid_sets_flag(self):
		name = self._inbound(message_id="IN2", from_lid=True)["name"]
		self.assertEqual(frappe.db.get_value("WhatsApp Message", name, "is_lid_sender"), 1)
		plain = self._inbound(message_id="IN3")["name"]
		self.assertEqual(frappe.db.get_value("WhatsApp Message", plain, "is_lid_sender"), 0)

	def test_account_event_temp_banned(self):
		api.account_event(self.account.session_id, "temp_banned", until="2026-10-31T12:00:00Z")
		row = frappe.db.get_value(
			"WhatsApp Account", self.account.name, ["status", "rate_limited_until"], as_dict=True
		)
		self.assertEqual(row.status, "RateLimited")
		self.assertIsNotNone(row.rate_limited_until)

	def test_temp_banned_does_not_override_paused(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Paused")
		api.account_event(self.account.session_id, "temp_banned", until="2026-10-31T12:00:00Z")
		msg = self._make_outgoing()
		api.message_status(status="Failed", local_message_id=msg.name, error_code="temp_banned")
		row = frappe.db.get_value(
			"WhatsApp Account", self.account.name, ["status", "rate_limited_until"], as_dict=True
		)
		self.assertEqual(row.status, "Paused")
		self.assertIsNone(row.rate_limited_until)

	def test_update_qr_active_ignored_when_paused(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Paused")
		api.update_qr(self.account.session_id, status="Active")
		api.update_qr(self.account.session_id, status="Disconnected")
		self.assertEqual(frappe.db.get_value("WhatsApp Account", self.account.name, "status"), "Paused")

	def test_update_qr_active_ignored_while_banned(self):
		future = add_to_date(now_datetime(), hours=1)
		frappe.db.set_value(
			"WhatsApp Account",
			self.account.name,
			{"status": "RateLimited", "rate_limited_until": future},
		)
		api.update_qr(self.account.session_id, status="Active")
		self.assertEqual(frappe.db.get_value("WhatsApp Account", self.account.name, "status"), "RateLimited")

		# Manual RateLimited (no expiry) is also sticky.
		frappe.db.set_value("WhatsApp Account", self.account.name, "rate_limited_until", None)
		api.update_qr(self.account.session_id, status="Active")
		self.assertEqual(frappe.db.get_value("WhatsApp Account", self.account.name, "status"), "RateLimited")

	def test_update_qr_active_clears_expired_ban(self):
		past = now_datetime() - timedelta(hours=1)
		frappe.db.set_value(
			"WhatsApp Account",
			self.account.name,
			{"status": "RateLimited", "rate_limited_until": past},
		)
		api.update_qr(self.account.session_id, status="Active")
		row = frappe.db.get_value(
			"WhatsApp Account", self.account.name, ["status", "rate_limited_until"], as_dict=True
		)
		self.assertEqual(row.status, "Active")
		self.assertIsNone(row.rate_limited_until)

	def test_bad_key_rejected(self):
		with patch("frappe.get_request_header", return_value="wrong"):
			with self.assertRaises(frappe.PermissionError):
				api.message_status(status="Sent", message_id="X")
			with self.assertRaises(frappe.PermissionError):
				api.account_event(self.account.session_id, "temp_banned")
