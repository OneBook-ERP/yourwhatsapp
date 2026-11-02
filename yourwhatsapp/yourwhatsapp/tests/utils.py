from unittest.mock import patch

import frappe
from frappe.tests import IntegrationTestCase


class LiveSafeTestCase(IntegrationTestCase):
	"""Base for every test module: tests run on the live site, so nothing may commit.

	Code under test that commits on purpose (adopt_pending_key, the key-change
	lock) hits `commit_mock` instead; assert on it where that matters. Anything
	written is rolled back in tearDown. Subclasses that override setUp or
	tearDown must call super()."""

	def setUp(self):
		super().setUp()
		commit = patch.object(frappe.db, "commit")
		self.commit_mock = commit.start()
		self.addCleanup(commit.stop)

	def tearDown(self):
		frappe.db.rollback()
		super().tearDown()
