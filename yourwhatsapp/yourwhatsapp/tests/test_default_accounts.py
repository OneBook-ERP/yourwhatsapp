from datetime import timedelta
from unittest.mock import MagicMock, patch

import frappe
from frappe.tests import IntegrationTestCase
from frappe.utils import now_datetime

from yourwhatsapp.patches.v1_0 import fill_default_whatsapp_accounts
from yourwhatsapp.yourwhatsapp import api, routing, tasks
from yourwhatsapp.yourwhatsapp.doctype.whatsapp_settings.whatsapp_settings import write_password

DEFAULT_FIELDS = ("default_outgoing_account", "default_incoming_account")
RELAY_KEY = "test-relay-key"


def relay_response(body):
	response = MagicMock()
	response.status_code = 200
	response.ok = True
	response.json.return_value = body
	return response


class DefaultsTestCase(IntegrationTestCase):
	"""Everything runs in a transaction that is rolled back: the live Settings defaults are never kept changed."""

	def setUp(self):
		settings = frappe.get_single("WhatsApp Settings")
		settings.relay_base_url = "http://relay.test"
		settings.save(ignore_permissions=True)
		write_password("relay_api_key", RELAY_KEY)
		key_patch = patch("frappe.get_request_header", return_value=RELAY_KEY)
		key_patch.start()
		self.addCleanup(key_patch.stop)
		self._set_defaults(None, None)
		self.account = self._make_account("Default Test A", "15550001001", status="Disconnected")

	def tearDown(self):
		frappe.db.rollback()

	def _make_account(self, name, phone_number, **fields):
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Account",
				"account_name": name,
				"phone_number": phone_number,
				"status": "Active",
				**fields,
			}
		).insert(ignore_permissions=True)

	def _set_defaults(self, outgoing, incoming):
		frappe.db.set_single_value("WhatsApp Settings", "default_outgoing_account", outgoing)
		frappe.db.set_single_value("WhatsApp Settings", "default_incoming_account", incoming)

	def _defaults(self):
		# Empty Link values read back as "" or None; normalise so tests compare either.
		return tuple(frappe.db.get_single_value("WhatsApp Settings", f) or None for f in DEFAULT_FIELDS)

	def _status(self, account=None):
		return frappe.db.get_value("WhatsApp Account", (account or self.account).name, "status")

	def _relay(self, status, call):
		with patch("requests.post", return_value=relay_response({"status": status})):
			call()


class TestDefaultAccounts(DefaultsTestCase):
	def test_first_active_fills_defaults(self):
		self.assertTrue(routing.set_account_active(self.account.name))
		self.assertEqual(self._status(), "Active")
		self.assertEqual(self._defaults(), (self.account.name, self.account.name))
		self.assertIsNotNone(frappe.db.get_value("WhatsApp Account", self.account.name, "connected_at"))

	def test_existing_default_kept(self):
		other = self._make_account("Default Test B", "15550001002")
		self._set_defaults(other.name, other.name)
		routing.set_account_active(self.account.name)
		self.assertEqual(self._defaults(), (other.name, other.name))

	def test_dangling_default_replaced(self):
		self._set_defaults("No Such Account", "No Such Account")
		routing.set_account_active(self.account.name)
		self.assertEqual(self._defaults(), (self.account.name, self.account.name))

	def test_paused_not_overridden(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Paused")
		self.assertFalse(routing.set_account_active(self.account.name))
		self.assertEqual(self._status(), "Paused")
		self.assertEqual(self._defaults(), (None, None))

	def test_held_ban_not_overridden(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "RateLimited")
		self.assertFalse(routing.set_account_active(self.account.name))
		self.assertEqual(self._status(), "RateLimited")
		self.assertEqual(self._defaults(), (None, None))

	def test_all_active_paths_use_helper(self):
		paths = {
			"update_qr": lambda: api.update_qr(self.account.session_id, status="Active"),
			"connect": lambda: self._relay("connected", self.account.connect),
			"resume": lambda: self._relay("connected", self.account.resume),
			"already_paired": lambda: self._already_paired(),
			"recover": lambda: self._recover(),
		}
		for label, run in paths.items():
			with self.subTest(path=label):
				frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Disconnected")
				self._set_defaults(None, None)
				run()
				self.assertEqual(self._status(), "Active")
				self.assertEqual(self._defaults(), (self.account.name, self.account.name))

	def _already_paired(self):
		conflict = relay_response({"detail": "already paired"})
		conflict.status_code = 409
		connected = relay_response({"status": "connected"})
		with patch("requests.post", side_effect=[conflict, connected]):
			self.account.pair_with_code()

	def _recover(self):
		frappe.db.set_value(
			"WhatsApp Account",
			self.account.name,
			{"status": "RateLimited", "rate_limited_until": now_datetime() - timedelta(hours=1)},
		)
		tasks.recover_rate_limited_accounts()

	def test_patch_fills_when_empty(self):
		fill_default_whatsapp_accounts.execute()
		for default in self._defaults():
			self.assertEqual(frappe.db.get_value("WhatsApp Account", default, "status"), "Active")

	def test_patch_keeps_valid_default_and_replaces_dangling(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Active")
		self._set_defaults("No Such Account", self.account.name)
		fill_default_whatsapp_accounts.execute()
		outgoing, incoming = self._defaults()
		self.assertTrue(frappe.db.exists("WhatsApp Account", outgoing))
		self.assertEqual(incoming, self.account.name)

	def test_routing_still_rotates_ignoring_default(self):
		first = self._make_account("Default Test B", "15550001002", last_used=now_datetime())
		second = self._make_account("Default Test C", "15550001003", last_used=now_datetime() - timedelta(hours=1))
		self._set_defaults(first.name, first.name)
		scoped = [
			a for a in routing.get_eligible_accounts() if a.name in (first.name, second.name)
		]
		with (
			patch.object(routing, "get_eligible_accounts", return_value=scoped),
			patch.object(frappe, "get_cached_value", return_value="Round Robin"),
		):
			self.assertEqual(routing.pick_account(), second.name)


class TestDefaultsFollowActive(DefaultsTestCase):
	"""CRM's is_whatsapp_enabled needs the default outgoing account to be Active, not just set."""

	def setUp(self):
		super().setUp()
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Active")
		self.other = self._make_account("Default Test B", "15550001002")
		self._set_defaults(self.account.name, self.account.name)

	def _assert_repointed(self):
		outgoing, incoming = self._defaults()
		for default in (outgoing, incoming):
			self.assertNotEqual(default, self.account.name)
			self.assertEqual(frappe.db.get_value("WhatsApp Account", default, "status"), "Active")
		from crm.api.whatsapp import is_whatsapp_enabled

		self.assertTrue(is_whatsapp_enabled())

	def test_transitions_away_from_active_repoint_defaults(self):
		transitions = {
			"mark_disconnected": lambda: routing.mark_disconnected(self.account.name),
			"mark_rate_limited": lambda: routing.mark_rate_limited(self.account.name),
			"pause": lambda: self._relay("ok", self.account.pause),
			"logout": lambda: self._relay("ok", self.account.logout),
			"update_qr": lambda: api.update_qr(self.account.session_id, status="Disconnected"),
			"account_event": lambda: api.account_event(self.account.session_id, "temp_banned"),
		}
		for label, run in transitions.items():
			with self.subTest(transition=label):
				frappe.db.set_value(
					"WhatsApp Account", self.account.name, {"status": "Active", "rate_limited_until": None}
				)
				self._set_defaults(self.account.name, self.account.name)
				run()
				self.assertNotEqual(self._status(), "Active")
				self._assert_repointed()

	def test_deleting_default_account_repoints(self):
		frappe.delete_doc("WhatsApp Account", self.account.name, ignore_permissions=True)
		self._assert_repointed()

	def test_fill_replaces_inactive_default(self):
		frappe.db.set_value("WhatsApp Account", self.other.name, "status", "Disconnected")
		self._set_defaults(self.other.name, self.other.name)
		routing.set_account_active(self.account.name)
		self.assertEqual(self._defaults(), (self.account.name, self.account.name))

	def test_no_active_account_left_keeps_defaults(self):
		frappe.db.set_value("WhatsApp Account", {"name": ["!=", self.account.name]}, "status", "Disconnected")
		routing.mark_disconnected(self.account.name)
		self.assertEqual(self._defaults(), (self.account.name, self.account.name))

	def test_background_default_write_keeps_settings_modified(self):
		modified = frappe.db.get_single_value("WhatsApp Settings", "modified")
		routing.mark_disconnected(self.account.name)
		self.assertEqual(frappe.db.get_single_value("WhatsApp Settings", "modified"), modified)
