import time
from unittest.mock import MagicMock, patch

import frappe
import requests
from frappe.utils import add_to_date, now_datetime

from yourwhatsapp import hooks
from yourwhatsapp.yourwhatsapp import alerts, relay_client, tasks
from yourwhatsapp.yourwhatsapp.doctype.whatsapp_settings.whatsapp_settings import write_password
from yourwhatsapp.yourwhatsapp.tests.utils import LiveSafeTestCase

RELAY = "http://relay.test"
RELAY_KEY = "test-relay-key"
JOB = "yourwhatsapp.yourwhatsapp.tasks.reconcile_account_status"


def relay_status(status):
	response = MagicMock()
	response.status_code = 200
	response.ok = True
	response.json.return_value = {"status": status}
	return response


class TestReconcileAccountStatus(LiveSafeTestCase):
	"""Runs in a rolled-back transaction; the relay is mocked. The job is limited
	to the test's own accounts (names=), so the live ones and their cache
	entries are never touched."""

	def setUp(self):
		super().setUp()
		settings = frappe.get_single("WhatsApp Settings")
		settings.relay_base_url = RELAY
		settings.save(ignore_permissions=True)
		write_password("relay_api_key", RELAY_KEY)
		self.relay = {}  # session_id -> relay status, or an exception to raise
		self.calls = []
		self.accounts = []

	def tearDown(self):
		for account in self.accounts:
			frappe.cache.delete_value(tasks.relay_down_key(account.name))
		super().tearDown()

	def _account(self, name, relay, **fields):
		doc = frappe.get_doc(
			{
				"doctype": "WhatsApp Account",
				"account_name": name,
				"phone_number": "1555000" + str(2000 + len(self.accounts)),
				"status": "Active",
				**fields,
			}
		).insert(ignore_permissions=True)
		self.accounts.append(doc)
		self.relay[doc.session_id] = relay
		return doc

	def _get(self, url, headers=None, timeout=None):
		self.calls.append((url, headers, timeout))
		session_id = url.rsplit("/", 2)[-2]
		answer = self.relay.get(session_id, requests.exceptions.ConnectionError("not a test account"))
		if isinstance(answer, Exception):
			raise answer
		if isinstance(answer, int):  # an HTTP error status
			response = relay_status("")
			response.status_code, response.ok = answer, False
			response.raise_for_status.side_effect = requests.exceptions.HTTPError(f"{answer} error")
			return response
		return relay_status(answer)

	def _run(self):
		with patch("requests.get", side_effect=self._get):
			tasks.reconcile_account_status(names=[a.name for a in self.accounts])

	def _down_since(self, account, seconds_ago):
		frappe.cache.set_value(tasks.relay_down_key(account.name), time.time() - seconds_ago)

	def _field(self, account, field="status"):
		return frappe.db.get_value("WhatsApp Account", account.name, field)

	def test_connected_marks_active(self):
		account = self._account("Reconcile Up", "connected", status="Disconnected")
		self._run()
		self.assertEqual(self._field(account), "Active")
		self.assertIsNotNone(self._field(account, "connected_at"))

	def test_calls_status_with_key_and_timeout(self):
		account = self._account("Reconcile Call", "connected")
		self._run()
		url, headers, timeout = next(c for c in self.calls if account.session_id in c[0])
		self.assertEqual(url, f"{RELAY}/session/{account.session_id}/status")
		self.assertEqual(headers, {"x-api-key": RELAY_KEY})
		self.assertEqual(timeout, 5)

	def test_down_marks_disconnected_after_job_interval(self):
		for status in ("disconnected", "not_started", "logged_out"):
			with self.subTest(relay=status):
				account = self._account(f"Reconcile Down {status}", status)
				self._run()
				self.assertEqual(self._field(account), "Active", "first seen down just now")
				self._run()
				self.assertEqual(self._field(account), "Active", "a quick second check is not a job interval")
				self._down_since(account, tasks.RECONCILE_MINUTES * 60 - 29)
				self._run()
				self.assertEqual(self._field(account), "Disconnected")

	def test_down_less_than_interval_kept(self):
		account = self._account("Reconcile Short", "disconnected")
		self._down_since(account, tasks.RECONCILE_MINUTES * 60 - 31)
		self._run()
		self.assertEqual(self._field(account), "Active")

	def test_first_seen_kept_across_checks(self):
		account = self._account("Reconcile First Seen", "disconnected")
		self._down_since(account, 100)
		first = frappe.cache.get_value(tasks.relay_down_key(account.name))
		self._run()
		self.assertEqual(frappe.cache.get_value(tasks.relay_down_key(account.name)), first)

	def test_recovery_between_checks_resets_down_time(self):
		account = self._account("Reconcile Blip", "disconnected")
		self._down_since(account, 600)
		self.relay[account.session_id] = "connected"
		self._run()
		self.assertIsNone(frappe.cache.get_value(tasks.relay_down_key(account.name)))
		self.relay[account.session_id] = "disconnected"
		self._run()
		self.assertEqual(self._field(account), "Active")

	def test_transitional_status_changes_nothing(self):
		account = self._account("Reconcile Connecting", "connecting", status="Disconnected")
		self._run()
		self._run()
		self.assertEqual(self._field(account), "Disconnected")

	def test_paused_never_overridden(self):
		up = self._account("Reconcile Paused Up", "connected", status="Paused")
		down = self._account("Reconcile Paused Down", "disconnected", status="Paused")
		self._run()
		self._run()
		self.assertEqual(self._field(up), "Paused")
		self.assertEqual(self._field(down), "Paused")

	def test_held_rate_limit_kept(self):
		account = self._account(
			"Reconcile Held",
			"connected",
			status="RateLimited",
			rate_limited_until=add_to_date(now_datetime(), hours=1),
		)
		self._run()
		self.assertEqual(self._field(account), "RateLimited")

	def test_unreachable_relay_changes_nothing(self):
		account = self._account("Reconcile Unreachable", requests.exceptions.ConnectTimeout("timed out"))
		before = self._field(account, "last_health_check")
		self._run()
		self._run()
		self.assertEqual(self._field(account), "Active")
		self.assertEqual(self._field(account, "last_health_check"), before)

	def test_unreachable_relay_stops_the_run(self):
		self._account("Reconcile Refused", requests.exceptions.ConnectionError("refused"))
		later = self._account("Reconcile Not Asked", "connected", status="Disconnected")
		self._run()
		self.assertEqual(len(self.calls), 1, "relay down for all: no point asking for the rest")
		self.assertEqual(self._field(later), "Disconnected")
		self.assertIsNone(self._field(later, "last_health_check"))

	def test_read_timeout_stops_the_run(self):
		self._account("Reconcile Slow", requests.exceptions.ReadTimeout("slow"))
		self._account("Reconcile Not Asked 2", "connected")
		self._run()
		self.assertEqual(len(self.calls), 1)

	def test_one_failure_does_not_stop_others(self):
		bad = self._account("Reconcile Bad Answer", 401)
		good = self._account("Reconcile After", "connected", status="Disconnected")
		with patch("frappe.log_error") as log_error:
			self._run()
		self.assertEqual(self._field(good), "Active")
		self.assertEqual(self._field(bad), "Active")
		self.assertIsNone(self._field(bad, "last_health_check"))
		log_error.assert_called_once()
		self.assertEqual(log_error.call_args.kwargs["reference_name"], bad.name)

	def test_client_outdated_account_kept_disconnected(self):
		account = self._account("Reconcile Outdated", "disconnected", status="Disconnected")
		self._down_since(account, 3600)
		self._run()
		self.assertEqual(self._field(account), "Disconnected")

	def test_session_status_helper(self):
		with patch("requests.get", side_effect=self._get) as get:
			self.relay["s-1"] = "connected"
			self.assertEqual(relay_client.session_status("s-1"), "connected")
		self.assertEqual(get.call_args.kwargs["timeout"], 5)

	def test_not_configured_changes_nothing(self):
		account = self._account("Reconcile Unconfigured", "connected", status="Disconnected")
		frappe.db.set_single_value("WhatsApp Settings", "relay_base_url", "")
		self._run()
		self.assertEqual(self.calls, [])
		self.assertEqual(self._field(account), "Disconnected")

	def test_pending_down_alerts_sent_even_without_relay(self):
		# A held-back down alert matters most while the relay is unreachable or unset.
		account = self._account("Reconcile Pending", "connected", status="Disconnected")
		frappe.db.set_single_value("WhatsApp Settings", "relay_base_url", "")
		with patch.object(alerts, "send_pending_down_alerts") as send_pending:
			self._run()
		send_pending.assert_called_once()
		self.assertIn(account.name, send_pending.call_args.args[0])

	def test_last_health_check_set_on_checked_accounts(self):
		account = self._account("Reconcile Health", "connected")
		self._run()
		checked = self._field(account, "last_health_check")
		self.assertIsNotNone(checked)
		self.assertLess(abs((now_datetime() - checked).total_seconds()), 60)

	def test_cron_registered_every_five_minutes(self):
		self.assertIn(JOB, hooks.scheduler_events["cron"]["*/5 * * * *"])
