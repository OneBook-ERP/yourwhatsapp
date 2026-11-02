import uuid
from unittest.mock import patch

import frappe

from yourwhatsapp.yourwhatsapp import alerts, api, routing
from yourwhatsapp.yourwhatsapp.tests.test_default_accounts import RELAY_KEY
from yourwhatsapp.yourwhatsapp.tests.utils import LiveSafeTestCase


class AlertsTestCase(LiveSafeTestCase):
	"""The real alert path. The base class keeps sendmail mocked and Redis keys in a cleared "test:" namespace."""

	real_alerts = True

	def setUp(self):
		super().setUp()
		self.account = f"alert-test-{uuid.uuid4().hex[:8]}"
		self.sendmail = self.sendmail_mock
		self._patch("yourwhatsapp.yourwhatsapp.alerts.has_outgoing_email_account", return_value=True)
		self.recipients = self._make_user("alert-admin@example.test", "WhatsApp Admin")

	def _patch(self, target, **kwargs):
		started = patch(target, **kwargs)
		mock = started.start()
		self.addCleanup(started.stop)
		return mock

	def _make_user(self, email, role, enabled=1):
		if not frappe.db.exists("Role", role):
			frappe.get_doc({"doctype": "Role", "role_name": role}).insert(ignore_permissions=True)
		user = frappe.get_doc(
			{"doctype": "User", "email": email, "first_name": email.split("@")[0], "enabled": enabled,
			 "send_welcome_email": 0, "roles": [{"role": role}]}
		).insert(ignore_permissions=True)
		return user.name

	def _bells(self, account=None):
		return frappe.get_all(
			"Notification Log",
			filters={"document_type": "WhatsApp Account", "document_name": account or self.account},
			fields=["for_user", "type", "subject"],
		)

	def _emailed(self):
		return [call.kwargs["subject"] for call in self.sendmail.call_args_list]

	def test_down_alert_bell_and_email(self):
		alerts.account_status_changed(self.account, "Active", "Disconnected")
		bells = [b for b in self._bells() if b.for_user == self.recipients]
		self.assertEqual(len(bells), 1)
		self.assertEqual(bells[0].type, "Alert")
		self.assertIn(self.account, bells[0].subject)
		self.sendmail.assert_called_once()
		kwargs = self.sendmail.call_args.kwargs
		self.assertIn(self.recipients, kwargs["recipients"])
		self.assertFalse(kwargs["now"])
		self.assertIn("Disconnected", kwargs["message"])

	def test_recipients_roles_enabled_only(self):
		manager = self._make_user("alert-manager@example.test", "System Manager")
		disabled = self._make_user("alert-disabled@example.test", "WhatsApp Admin", enabled=0)
		other = self._make_user("alert-other@example.test", "Sales User")
		found = alerts.alert_recipients()
		self.assertIn(self.recipients, found)
		self.assertIn(manager, found)
		self.assertNotIn(disabled, found)
		self.assertNotIn(other, found)
		self.assertNotIn("Guest", found)

	def test_administrator_only_with_email(self):
		for email in (None, "admin@example.com"):
			with patch.object(frappe.db, "get_value", wraps=frappe.db.get_value) as get_value:
				get_value.side_effect = lambda doctype, name=None, *a, **k: (
					(1, email) if name == "Administrator" and doctype == "User" else frappe.db.get_value.__wrapped__(doctype, name, *a, **k)
				)
				self.assertNotIn("Administrator", alerts.alert_recipients())
		with patch.object(alerts, "_administrator_email", return_value="boss@company.test"):
			self.assertIn("Administrator", alerts.alert_recipients())

	def test_flap_throttled(self):
		alerts.account_status_changed(self.account, "Active", "Disconnected")
		alerts.account_status_changed(self.account, "Disconnected", "Active")
		self.sendmail.reset_mock()
		before = len(self._bells())
		alerts.account_status_changed(self.account, "Active", "Disconnected")
		self.sendmail.assert_not_called()
		self.assertEqual(len(self._bells()), before)

	def test_recovery_only_after_down(self):
		alerts.account_status_changed(self.account, "Disconnected", "Active")
		self.sendmail.assert_not_called()
		self.assertEqual(self._bells(), [])

	def test_recovery_not_throttled(self):
		alerts.account_status_changed(self.account, "Active", "Disconnected")
		alerts.account_status_changed(self.account, "Disconnected", "Active")
		self.assertEqual(len(self._emailed()), 2)
		self.assertIn("back online", self._emailed()[1])
		# a second recovery for the same outage is not sent
		alerts.account_status_changed(self.account, "Disconnected", "Active")
		self.assertEqual(len(self._emailed()), 2)

	def test_manual_pause_silent(self):
		alerts.account_status_changed(self.account, "Active", "Paused", reason="manual")
		self.sendmail.assert_not_called()
		self.assertEqual(self._bells(), [])
		self.assertTrue(frappe.cache.get_value(alerts.alert_key(self.account, "manual")))

	def test_logout_then_relay_disconnect_silent(self):
		alerts.account_status_changed(self.account, "Active", "Disconnected", reason="manual")
		alerts.account_status_changed(self.account, "Active", "Disconnected")
		self.sendmail.assert_not_called()
		self.assertEqual(self._bells(), [])

	def test_rate_limit_expiry_is_recovery(self):
		alerts.account_status_changed(self.account, "Active", "RateLimited")
		self.assertIn("RateLimited", self.sendmail.call_args.kwargs["message"])
		alerts.account_status_changed(self.account, "RateLimited", "Active")
		self.assertIn("back online", self._emailed()[1])

	def test_non_active_transitions_silent(self):
		alerts.account_status_changed(self.account, "Disconnected", "RateLimited")
		alerts.account_status_changed(self.account, "Active", "Active")
		self.sendmail.assert_not_called()

	def test_client_outdated_reason_in_alert(self):
		alerts.account_status_changed(self.account, "Active", "Disconnected", reason="client outdated")
		self.assertIn("client outdated", self.sendmail.call_args.kwargs["message"])
		self.assertIn("client outdated", [b for b in self._bells() if b.for_user == self.recipients][0].subject)

	def test_no_email_account_bell_only(self):
		self._patch("yourwhatsapp.yourwhatsapp.alerts.has_outgoing_email_account", return_value=False)
		with patch("frappe.log_error") as log_error:
			alerts.account_status_changed(self.account, "Active", "Disconnected")
			alerts.account_status_changed(self.account, "Disconnected", "Active")
		self.sendmail.assert_not_called()
		self.assertEqual(len([b for b in self._bells() if b.for_user == self.recipients]), 2)
		log_error.assert_called_once()


	def test_recipient_without_email_gets_bell_only(self):
		no_email = self._make_user("alert-noemail@example.test", "WhatsApp Admin")
		with patch.object(alerts, "_user_email", side_effect=lambda user: None if user == no_email else user):
			alerts.account_status_changed(self.account, "Active", "Disconnected")
		self.assertIn(no_email, [b.for_user for b in self._bells()])
		self.assertNotIn(no_email, self.sendmail.call_args.kwargs["recipients"])
		self.assertIn(self.recipients, self.sendmail.call_args.kwargs["recipients"])

	def test_email_link_is_escaped(self):
		alerts._email([self.recipients], "s", "d", 'http://x.test/a"b')
		self.assertIn("a&quot;b", self.sendmail.call_args.kwargs["message"])

	def test_down_sent_has_ttl(self):
		with patch.object(frappe.cache, "set_value", wraps=frappe.cache.set_value) as set_value:
			alerts.account_status_changed(self.account, "Active", "Disconnected")
		ttls = {c.args[0]: c.kwargs.get("expires_in_sec") for c in set_value.call_args_list}
		self.assertEqual(ttls[alerts.alert_key(self.account, "down_sent")], alerts.DOWN_SENT_SECONDS)

	def test_alert_failure_never_raises(self):
		with patch.object(alerts, "_send_down", side_effect=RuntimeError), patch(
			"frappe.log_error", side_effect=RuntimeError
		):
			alerts.account_status_changed(self.account, "Active", "Disconnected")

	def test_deleted_account_name_reuse_has_no_spurious_recovery(self):
		account = frappe.get_doc(
			{"doctype": "WhatsApp Account", "account_name": "Alert Reuse", "phone_number": "15550009002",
			 "status": "Active"}
		).insert(ignore_permissions=True)
		alerts.account_status_changed(account.name, "Active", "Disconnected")
		frappe.delete_doc("WhatsApp Account", account.name, force=True, ignore_permissions=True)
		self.sendmail.reset_mock()
		alerts.account_status_changed(account.name, "Disconnected", "Active")
		self.sendmail.assert_not_called()


class AlertWiringTestCase(LiveSafeTestCase):
	"""The routing helpers, account events and admin actions reach alerts.account_status_changed."""

	def setUp(self):
		super().setUp()
		self.changed = self.alerts_mock
		self.account = frappe.get_doc(
			{"doctype": "WhatsApp Account", "account_name": "Alert Wiring", "phone_number": "15550009001",
			 "status": "Active"}
		).insert(ignore_permissions=True)

	def test_mark_disconnected_reports_old_status(self):
		self.assertTrue(routing.mark_disconnected(self.account.name, reason="client outdated"))
		self.changed.assert_called_once_with(self.account.name, "Active", "Disconnected", "client outdated")

	def test_mark_disconnected_held_by_pause_is_silent(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Paused")
		self.assertFalse(routing.mark_disconnected(self.account.name))
		self.changed.assert_not_called()

	def test_mark_disconnected_already_disconnected_does_not_alert(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Disconnected")
		routing.mark_disconnected(self.account.name)
		# The hook still runs, but with old == new the real function ignores it.
		self.changed.assert_called_once_with(self.account.name, "Disconnected", "Disconnected", None)

	def test_mark_rate_limited_reports(self):
		self.assertTrue(routing.mark_rate_limited(self.account.name))
		self.changed.assert_called_once_with(self.account.name, "Active", "RateLimited", None)

	def test_mark_rate_limited_paused_is_silent(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Paused")
		self.assertFalse(routing.mark_rate_limited(self.account.name))
		self.changed.assert_not_called()

	def test_set_account_active_reports(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Disconnected")
		self.assertTrue(routing.set_account_active(self.account.name))
		self.changed.assert_called_once_with(self.account.name, "Disconnected", "Active", None)

	def test_set_account_active_refused_is_silent(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Paused")
		self.assertFalse(routing.set_account_active(self.account.name))
		self.changed.assert_not_called()

	def test_client_outdated_event_passes_reason(self):
		with patch("frappe.log_error"), patch("frappe.get_request_header", return_value=RELAY_KEY), patch.object(
			api, "_verify_relay_key"
		):
			api.account_event(self.account.session_id, "client_outdated")
		self.changed.assert_called_once_with(self.account.name, "Active", "Disconnected", "client outdated")

	def test_pause_and_logout_are_manual(self):
		with patch.object(type(self.account), "_relay_post", return_value={}):
			self.account.pause()
			self.changed.assert_called_with(self.account.name, "Active", "Paused", reason="manual")
			self.account.logout()
		self.changed.assert_called_with(self.account.name, "Paused", "Disconnected", reason="manual")
