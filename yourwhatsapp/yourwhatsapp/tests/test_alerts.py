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

	def _change(self, old, new, reason=None, account=None):
		"""One status change in its own committed request."""
		alerts.account_status_changed(account or self.account, old, new, reason)
		self._commit()

	def _commit(self):
		# frappe.db.commit is mocked; alert state is written by its after-commit callbacks.
		frappe.db.after_commit.run()

	def _bells(self, account=None):
		return frappe.get_all(
			"Notification Log",
			filters={"document_type": "WhatsApp Account", "document_name": account or self.account},
			fields=["for_user", "type", "subject"],
		)

	def _emailed(self):
		return [call.kwargs["subject"] for call in self.sendmail.call_args_list]

	def test_down_alert_bell_and_email(self):
		self._change("Active", "Disconnected")
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

	def test_administrator_gets_bell_whenever_enabled(self):
		with patch.object(alerts, "_administrator_enabled", return_value=True):
			self.assertIn("Administrator", alerts.alert_recipients())
		with patch.object(alerts, "_administrator_enabled", return_value=False):
			self.assertNotIn("Administrator", alerts.alert_recipients())

	def test_administrator_emailed_only_with_real_address(self):
		for email, emailed in ((None, False), ("admin@example.com", False), ("boss@company.test", True)):
			with self.subTest(email=email):
				self.sendmail.reset_mock()
				addresses = {self.recipients: self.recipients, "Administrator": email}
				with patch.object(alerts, "_administrator_enabled", return_value=True), patch.object(
					alerts, "_stored_email", side_effect=lambda user: addresses.get(user)
				):
					alerts._notify(self.account, "subject", "detail")
				self.assertIn("Administrator", [b.for_user for b in self._bells()])
				sent_to = self.sendmail.call_args.kwargs["recipients"]
				self.assertEqual("boss@company.test" in sent_to, emailed)
				self.assertNotIn("admin@example.com", sent_to)
				frappe.db.delete("Notification Log", {"document_name": self.account})

	def test_no_recipients_logged_once_a_day(self):
		with patch.object(alerts, "alert_recipients", return_value=[]):
			alerts._notify(self.account, "subject", "detail")
			alerts._notify(self.account, "subject", "detail")
		self.assertEqual(self._bells(), [])
		self.sendmail.assert_not_called()
		self.log_error_mock.assert_called_once()
		self.assertIn("give a user System Manager or WhatsApp Admin", self.log_error_mock.call_args.kwargs["message"])

	def test_flap_throttled(self):
		self._change("Active", "Disconnected")
		self._change("Disconnected", "Active")
		self.sendmail.reset_mock()
		before = len(self._bells())
		self._change("Active", "Disconnected")
		self.sendmail.assert_not_called()
		self.assertEqual(len(self._bells()), before)

	def test_alert_state_written_only_after_commit(self):
		alerts.account_status_changed(self.account, "Active", "Disconnected")
		for kind in ("down", "down_sent"):
			self.assertIsNone(frappe.cache.get_value(alerts.alert_key(self.account, kind)))
		self._commit()
		for kind in ("down", "down_sent"):
			self.assertTrue(frappe.cache.get_value(alerts.alert_key(self.account, kind)))

	def test_rolled_back_alert_is_sent_again_on_retry(self):
		alerts.account_status_changed(self.account, "Active", "Disconnected")
		frappe.db.rollback()  # the callback failed later in its request
		self._commit()
		self.recipients = self._make_user("alert-admin@example.test", "WhatsApp Admin")  # rolled back too
		for kind in ("down", "down_sent"):
			self.assertIsNone(frappe.cache.get_value(alerts.alert_key(self.account, kind)))
		self.assertEqual(self._bells(), [])
		self._change("Active", "Disconnected")  # the relay's outbox retries it
		self.assertEqual(len([b for b in self._bells() if b.for_user == self.recipients]), 1)
		self.assertEqual(self.sendmail.call_count, 2)

	def test_recovery_clears_down_sent_only_after_commit(self):
		self._change("Active", "Disconnected")
		alerts.account_status_changed(self.account, "Disconnected", "Active")
		frappe.db.rollback()
		self.assertTrue(frappe.cache.get_value(alerts.alert_key(self.account, "down_sent")))

	def test_recovery_only_after_down(self):
		self._change("Disconnected", "Active")
		self.sendmail.assert_not_called()
		self.assertEqual(self._bells(), [])

	def test_recovery_not_throttled(self):
		self._change("Active", "Disconnected")
		self._change("Disconnected", "Active")
		self.assertEqual(len(self._emailed()), 2)
		self.assertIn("back online", self._emailed()[1])
		# a second recovery for the same outage is not sent
		self._change("Disconnected", "Active")
		self.assertEqual(len(self._emailed()), 2)

	def test_manual_pause_silent(self):
		self._change("Active", "Paused", reason="manual")
		self.sendmail.assert_not_called()
		self.assertEqual(self._bells(), [])
		self.assertTrue(frappe.cache.get_value(alerts.alert_key(self.account, "manual")))

	def test_logout_then_relay_disconnect_silent(self):
		self._change("Active", "Disconnected", reason="manual")
		self._change("Active", "Disconnected")
		self.sendmail.assert_not_called()
		self.assertEqual(self._bells(), [])

	def test_rate_limit_expiry_is_recovery(self):
		self._change("Active", "RateLimited")
		self.assertIn("RateLimited", self.sendmail.call_args.kwargs["message"])
		self._change("RateLimited", "Active")
		self.assertIn("back online", self._emailed()[1])

	def test_non_active_transitions_silent(self):
		self._change("Disconnected", "RateLimited")
		self._change("Active", "Active")
		self.sendmail.assert_not_called()

	def test_client_outdated_reason_in_alert(self):
		self._change("Active", "Disconnected", reason="client outdated")
		self.assertIn("client outdated", self.sendmail.call_args.kwargs["message"])
		self.assertIn("client outdated", [b for b in self._bells() if b.for_user == self.recipients][0].subject)

	def test_no_email_account_bell_only(self):
		self._patch("yourwhatsapp.yourwhatsapp.alerts.has_outgoing_email_account", return_value=False)
		with patch("frappe.log_error") as log_error:
			self._change("Active", "Disconnected")
			self._change("Disconnected", "Active")
		self.sendmail.assert_not_called()
		self.assertEqual(len([b for b in self._bells() if b.for_user == self.recipients]), 2)
		log_error.assert_called_once()


	def test_recipient_without_email_gets_bell_only(self):
		no_email = self._make_user("alert-noemail@example.test", "WhatsApp Admin")
		with patch.object(alerts, "_stored_email", side_effect=lambda user: None if user == no_email else user):
			self._change("Active", "Disconnected")
		self.assertIn(no_email, [b.for_user for b in self._bells()])
		self.assertNotIn(no_email, self.sendmail.call_args.kwargs["recipients"])
		self.assertIn(self.recipients, self.sendmail.call_args.kwargs["recipients"])

	def test_email_link_is_escaped(self):
		alerts._email([self.recipients], "s", "d", 'http://x.test/a"b')
		self.assertIn("a&quot;b", self.sendmail.call_args.kwargs["message"])

	def test_down_sent_has_ttl(self):
		with patch.object(frappe.cache, "set_value", wraps=frappe.cache.set_value) as set_value:
			self._change("Active", "Disconnected")
		ttls = {c.args[0]: c.kwargs.get("expires_in_sec") for c in set_value.call_args_list}
		self.assertEqual(ttls[alerts.alert_key(self.account, "down_sent")], alerts.DOWN_SENT_SECONDS)

	def test_alert_failure_never_raises(self):
		with patch.object(alerts, "_send_down", side_effect=RuntimeError), patch(
			"frappe.log_error", side_effect=RuntimeError
		):
			self._change("Active", "Disconnected")

	def _relay_account(self, status="Active"):
		account = frappe.get_doc(
			{"doctype": "WhatsApp Account", "account_name": f"Alert Relay {uuid.uuid4().hex[:6]}",
			 "phone_number": "15550009003", "status": status}
		).insert(ignore_permissions=True)
		self._patch("yourwhatsapp.yourwhatsapp.api._verify_relay_key")
		return account

	def test_update_qr_disconnected_alerts(self):
		# The relay reports an unlinked (logged out) device as update_qr Disconnected.
		account = self._relay_account()
		api.update_qr(account.session_id, status="Disconnected")
		self.assertEqual(len(self._emailed()), 1)
		self.assertIn("is down", self._emailed()[0])
		self.assertEqual(len([b for b in self._bells(account.name) if b.for_user == self.recipients]), 1)

	def test_client_outdated_after_update_qr_alerts_once_with_reason(self):
		account = self._relay_account()
		api.update_qr(account.session_id, status="Disconnected", reason="client outdated")
		self._commit()
		with patch("frappe.log_error"):
			api.account_event(account.session_id, "client_outdated")
		self.assertEqual(len(self._emailed()), 1)
		self.assertIn("client outdated", self._emailed()[0])

	def test_client_outdated_on_disconnected_account_alerts_once(self):
		# Down without an alert (e.g. before alerts existed): client_outdated still tells the admins.
		account = self._relay_account(status="Disconnected")
		with patch("frappe.log_error"):
			api.account_event(account.session_id, "client_outdated")
			self._commit()
			api.account_event(account.session_id, "client_outdated")
		self.assertEqual(len(self._emailed()), 1)
		self.assertIn("client outdated", self._emailed()[0])

	def test_deleted_account_name_reuse_has_no_spurious_recovery(self):
		account = frappe.get_doc(
			{"doctype": "WhatsApp Account", "account_name": "Alert Reuse", "phone_number": "15550009002",
			 "status": "Active"}
		).insert(ignore_permissions=True)
		self._change("Active", "Disconnected", account=account.name)
		frappe.delete_doc("WhatsApp Account", account.name, force=True, ignore_permissions=True)
		self.sendmail.reset_mock()
		self._change("Disconnected", "Active", account=account.name)
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
			self.changed.assert_called_with(self.account.name, "Active", "Paused", "manual")
			self.account.logout()
		self.changed.assert_called_with(self.account.name, "Paused", "Disconnected", "manual")

	def _update_qr(self, **kwargs):
		with patch.object(api, "_verify_relay_key"):
			api.update_qr(self.account.session_id, **kwargs)

	def test_update_qr_disconnected_goes_through_routing(self):
		self._update_qr(status="Disconnected")
		self.changed.assert_called_once_with(self.account.name, "Active", "Disconnected", None)

	def test_update_qr_passes_relay_reason(self):
		self._update_qr(status="Disconnected", reason="logged out")
		self.changed.assert_called_once_with(self.account.name, "Active", "Disconnected", "logged out")

	def test_update_qr_disconnected_held_by_pause_is_silent(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Paused")
		self._update_qr(status="Disconnected")
		self.changed.assert_not_called()
		self.assertEqual(frappe.db.get_value("WhatsApp Account", self.account.name, "status"), "Paused")

	def test_client_outdated_on_disconnected_reports_down(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Disconnected")
		with patch("frappe.log_error"), patch.object(api, "_verify_relay_key"):
			api.account_event(self.account.session_id, "client_outdated")
		self.report_down_mock.assert_called_once_with(self.account.name, "Disconnected", "client outdated")

	def test_client_outdated_on_active_does_not_report_twice(self):
		with patch("frappe.log_error"), patch.object(api, "_verify_relay_key"):
			api.account_event(self.account.session_id, "client_outdated")
		self.report_down_mock.assert_not_called()

	def test_resume_lifts_pause_silently(self):
		frappe.db.set_value("WhatsApp Account", self.account.name, "status", "Paused")
		with patch.object(type(self.account), "_relay_post", return_value={}):
			self.account.resume()
		self.changed.assert_called_once_with(self.account.name, "Paused", "Disconnected", None)

	def test_resume_keeps_active_account_active(self):
		with patch.object(type(self.account), "_relay_post", return_value={}):
			self.account.resume()
		self.changed.assert_not_called()
		self.assertEqual(frappe.db.get_value("WhatsApp Account", self.account.name, "status"), "Active")

	def test_form_status_edit_is_manual(self):
		account = frappe.get_doc("WhatsApp Account", self.account.name)
		account.status = "RateLimited"
		account.save(ignore_permissions=True)
		self.changed.assert_called_once_with(self.account.name, "Active", "RateLimited", "manual")

	def test_form_save_without_status_change_is_silent(self):
		account = frappe.get_doc("WhatsApp Account", self.account.name)
		account.proxy = "socks5://proxy.test:1080"
		account.save(ignore_permissions=True)
		self.changed.assert_not_called()
