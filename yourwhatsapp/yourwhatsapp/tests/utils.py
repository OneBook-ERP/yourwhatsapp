from unittest.mock import MagicMock, patch

import frappe
from frappe.tests import IntegrationTestCase

from yourwhatsapp.yourwhatsapp import alerts

TEST_ALERT_KEY_PREFIX = "test:yourwhatsapp:alert:"


class LiveSafeTestCase(IntegrationTestCase):
	"""Base for every test module: tests run on the live site, so nothing may commit.

	Code under test that commits on purpose (adopt_pending_key, the key-change
	lock) hits `commit_mock` instead; assert on it where that matters. Anything
	written is rolled back in tearDown. Subclasses that override setUp or
	tearDown must call super().

	Health alerts are isolated too: `frappe.sendmail` is a mock (`sendmail_mock`)
	and alert Redis keys live in a "test:" namespace that tearDown clears, so no
	test can mail anyone or write a real `yourwhatsapp:alert:*` key. Alert
	delivery is a mock (`alerts_mock`) unless the class sets `real_alerts = True`."""

	real_alerts = False

	def setUp(self):
		super().setUp()
		commit = patch.object(frappe.db, "commit")
		self.commit_mock = commit.start()
		self.addCleanup(commit.stop)
		self.sendmail_mock = self._start_patch(patch("frappe.sendmail"))
		self._alert_keys = set()
		self._start_patch(patch.object(alerts, "KEY_PREFIX", TEST_ALERT_KEY_PREFIX))
		real_alert_key = alerts.alert_key

		def tracked_alert_key(account, kind):
			key = real_alert_key(account, kind)
			self._alert_keys.add(key)
			return key

		self._start_patch(patch.object(alerts, "alert_key", tracked_alert_key))
		self.alerts_mock = None
		if not self.real_alerts:
			self.alerts_mock = self._start_patch(
				patch.object(alerts, "account_status_changed", MagicMock(name="account_status_changed"))
			)

	def _start_patch(self, started):
		mock = started.start()
		self.addCleanup(started.stop)
		return mock

	def tearDown(self):
		for key in self._alert_keys:
			frappe.cache.delete_value(key)
		frappe.db.rollback()
		super().tearDown()
