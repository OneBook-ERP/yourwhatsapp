import json
from unittest.mock import MagicMock, patch

import frappe
import requests
from frappe.tests import IntegrationTestCase
from frappe.utils import add_to_date, now_datetime

from yourwhatsapp.yourwhatsapp import api, relay_client, tasks

RELAY_KEY = "test-relay-key"


def relay_response(status_code, body=None):
	response = MagicMock()
	response.status_code = status_code
	response.json.return_value = body or {}
	return response


class TestRelayClient(IntegrationTestCase):
	def setUp(self):
		settings = frappe.get_single("WhatsApp Settings")
		settings.relay_base_url = "http://relay.test"
		settings.relay_api_key = RELAY_KEY
		settings.save(ignore_permissions=True)

		for patcher in (
			patch("frappe.enqueue"),
			patch("frappe.get_request_header", return_value=RELAY_KEY),
		):
			patcher.start()
			self.addCleanup(patcher.stop)

		# Start from a clean routing pool so only this test's accounts are eligible.
		frappe.db.set_value("WhatsApp Account", {}, "status", "Disconnected")
		self.account_a = self._make_account("Client Test A")
		self.account_b = self._make_account("Client Test B")

	def tearDown(self):
		frappe.db.rollback()

	def _make_account(self, name, **fields):
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Account",
				"account_name": name,
				"phone_number": "15550001111",
				"status": "Active",
				**fields,
			}
		).insert(ignore_permissions=True)

	def _make_message(self, account=None, to="15551234567"):
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Message",
				"type": "Outgoing",
				"whatsapp_account": (account or self.account_a).name,
				"to": to,
				"content_type": "text",
				"message": "hi",
			}
		).insert(ignore_permissions=True)

	def _row(self, name):
		return frappe.db.get_value(
			"WhatsApp Message", name, ["status", "whatsapp_account", "error_reason", "attempted_accounts"], as_dict=True
		)

	def _account_value(self, account, field):
		return frappe.db.get_value("WhatsApp Account", account.name, field)

	def _send(self, msg, *responses):
		with patch("requests.post", side_effect=list(responses)) as post:
			relay_client.send_message(msg.name)
		return post

	def test_202_leaves_queued(self):
		msg = self._make_message()
		post = self._send(msg, relay_response(202, {"state": "queued"}))
		self.assertEqual(self._row(msg.name).status, "Queued")
		body = post.call_args.kwargs["json"]
		self.assertEqual(body["local_message_id"], msg.name)
		self.assertEqual(body["session_id"], self.account_a.session_id)
		self.assertIs(body["to_is_lid"], False)
		self.assertEqual(post.call_args.kwargs["timeout"], 30)
		self.assertEqual(self._account_value(self.account_a, "daily_message_count"), 0)

	def test_duplicate_noop(self):
		msg = self._make_message()
		post = self._send(msg, relay_response(200, {"error": "duplicate", "state": "sent", "message_id": "X"}))
		self.assertEqual(post.call_count, 1)
		row = self._row(msg.name)
		self.assertEqual((row.status, row.whatsapp_account), ("Queued", self.account_a.name))

	def test_422_failed_no_failover(self):
		msg = self._make_message()
		post = self._send(msg, relay_response(422, {"error": "invalid_number"}))
		row = self._row(msg.name)
		self.assertEqual(row.status, "Failed")
		self.assertIn("invalid_number", row.error_reason)
		self.assertEqual(post.call_count, 1)

	def test_409_fails_over_to_next_account(self):
		msg = self._make_message()
		post = self._send(msg, relay_response(409, {"error": "session_unavailable"}), relay_response(202))
		row = self._row(msg.name)
		self.assertEqual((row.status, row.whatsapp_account), ("Queued", self.account_b.name))
		self.assertEqual(json.loads(row.attempted_accounts), [self.account_a.name, self.account_b.name])
		self.assertEqual(post.call_args.kwargs["json"]["session_id"], self.account_b.session_id)

	def test_429_sets_rate_limited_and_fails_over(self):
		msg = self._make_message()
		self._send(msg, relay_response(429, {"error": "temp_banned"}), relay_response(202))
		self.assertEqual(self._account_value(self.account_a, "status"), "RateLimited")
		self.assertEqual(self._row(msg.name).whatsapp_account, self.account_b.name)

	def test_timeout_retries_same_account_once(self):
		msg = self._make_message()
		post = self._send(msg, requests.exceptions.ConnectTimeout(), relay_response(200, {"error": "duplicate"}))
		self.assertEqual(post.call_count, 2)
		self.assertEqual(self._row(msg.name).whatsapp_account, self.account_a.name)
		self.assertEqual(
			{c.kwargs["json"]["session_id"] for c in post.call_args_list}, {self.account_a.session_id}
		)

	def test_second_timeout_fails_over(self):
		msg = self._make_message()
		post = self._send(
			msg, requests.exceptions.ReadTimeout(), requests.exceptions.ReadTimeout(), relay_response(202)
		)
		self.assertEqual(post.call_count, 3)
		self.assertEqual(self._row(msg.name).whatsapp_account, self.account_b.name)

	def test_connection_error_fails_over_without_retry(self):
		msg = self._make_message()
		post = self._send(msg, requests.exceptions.ConnectionError(), relay_response(202))
		self.assertEqual(post.call_count, 2)
		self.assertEqual(self._row(msg.name).whatsapp_account, self.account_b.name)

	def test_all_accounts_exhausted_failed(self):
		msg = self._make_message()
		self._send(msg, relay_response(409, {"error": "session_unavailable"}), relay_response(409))
		row = self._row(msg.name)
		self.assertEqual(row.status, "Failed")
		self.assertIn("no other WhatsApp Account", row.error_reason)

	def test_async_failure_fails_over_excluding_attempted(self):
		msg = self._make_message()
		self._send(msg, relay_response(202))
		api.message_status(status="Failed", local_message_id=msg.name, error_code="send_failed", error_reason="boom")
		self.assertEqual(self._row(msg.name).status, "Failed")

		with patch("requests.post", return_value=relay_response(202)) as post:
			relay_client.handle_async_failure(msg.name, "send_failed")
		row = self._row(msg.name)
		self.assertEqual((row.status, row.whatsapp_account), ("Queued", self.account_b.name))
		self.assertEqual(post.call_args.kwargs["json"]["session_id"], self.account_b.session_id)

		# B fails too: nothing left that was not attempted.
		api.message_status(status="Failed", local_message_id=msg.name, error_code="send_failed")
		with patch("requests.post") as post:
			relay_client.handle_async_failure(msg.name, "send_failed")
		post.assert_not_called()
		self.assertEqual(self._row(msg.name).status, "Failed")

	def test_reply_to_lid_sender_sets_flag(self):
		frappe.get_doc(
			{
				"doctype": "WhatsApp Message",
				"type": "Incoming",
				"whatsapp_account": self.account_a.name,
				"from": "99999111",
				"content_type": "text",
				"message": "hello",
				"is_lid_sender": 1,
			}
		).insert(ignore_permissions=True)
		msg = self._make_message(to="99999111")
		post = self._send(msg, relay_response(202))
		self.assertIs(post.call_args.kwargs["json"]["to_is_lid"], True)

	def test_sent_callback_increments_count_atomically(self):
		msg = self._make_message()
		api.message_status(status="Sent", local_message_id=msg.name, message_id="WAID1")
		self.assertEqual(self._account_value(self.account_a, "daily_message_count"), 1)
		# A repeated Sent callback must not double count.
		api.message_status(status="Sent", local_message_id=msg.name, message_id="WAID1")
		self.assertEqual(self._account_value(self.account_a, "daily_message_count"), 1)
		relay_client.increment_daily_count(self.account_a.name)
		self.assertEqual(self._account_value(self.account_a, "daily_message_count"), 2)

	def test_recover_rate_limited_after_until(self):
		past = add_to_date(now_datetime(), minutes=-5)
		future = add_to_date(now_datetime(), hours=1)
		expired = self._make_account("Client Expired", status="RateLimited", rate_limited_until=past)
		pending = self._make_account("Client Pending", status="RateLimited", rate_limited_until=future)
		tasks.recover_rate_limited_accounts()
		self.assertEqual(self._account_value(expired, "status"), "Active")
		self.assertIsNone(self._account_value(expired, "rate_limited_until"))
		self.assertEqual(self._account_value(pending, "status"), "RateLimited")

	def test_manual_rate_limited_not_recovered(self):
		manual = self._make_account("Client Manual", status="RateLimited")
		tasks.recover_rate_limited_accounts()
		self.assertEqual(self._account_value(manual, "status"), "RateLimited")
