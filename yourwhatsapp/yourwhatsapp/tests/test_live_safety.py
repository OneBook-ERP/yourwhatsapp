import ast
import pathlib
import unittest
from unittest.mock import MagicMock

import frappe

from yourwhatsapp.yourwhatsapp import alerts
from yourwhatsapp.yourwhatsapp.tests.utils import LiveSafeTestCase

TESTS_DIR = pathlib.Path(__file__).parent
BASE = "LiveSafeTestCase"


def _frappe_case_classes(path):
	"""Classes in a test module whose bases name a Frappe test case (directly or via another class there)."""
	classes = {node.name: node for node in ast.parse(path.read_text()).body if isinstance(node, ast.ClassDef)}

	def base_names(node):
		return {b.attr if isinstance(b, ast.Attribute) else getattr(b, "id", "") for b in node.bases}

	def is_frappe_case(node, seen=()):
		names = base_names(node)
		if names & {"IntegrationTestCase", "FrappeTestCase", BASE}:
			return True
		return any(
			is_frappe_case(classes[n], seen + (node.name,)) for n in names if n in classes and n not in seen
		)

	def is_live_safe(node, seen=()):
		names = base_names(node)
		if BASE in names:
			return True
		return any(
			is_live_safe(classes[n], seen + (node.name,)) for n in names if n in classes and n not in seen
		)

	return [(name, is_live_safe(node)) for name, node in classes.items() if is_frappe_case(node)]


class TestLiveSafety(unittest.TestCase):
	"""Tests run on the live site; a module that can commit would write test rows into it."""

	def test_all_modules_use_live_safe_base(self):
		offenders = [
			f"{path.name}::{name}"
			for path in sorted(TESTS_DIR.glob("test_*.py"))
			for name, safe in _frappe_case_classes(path)
			if not safe
		]
		self.assertEqual(offenders, [])


class TestSideEffectIsolation(LiveSafeTestCase):
	"""Side effects a rollback can't undo are mocked for every module."""

	def test_error_log_is_mocked(self):
		# tabError Log is MyISAM: a real insert survives the rollback.
		self.assertIs(frappe.log_error, self.log_error_mock)
		self.assertIsInstance(frappe.log_error, MagicMock)

	def test_realtime_is_mocked(self):
		# CRM broadcasts WhatsApp Message rows to every open desk.
		self.assertIs(frappe.publish_realtime, self.publish_realtime_mock)
		self.assertIsInstance(frappe.publish_realtime, MagicMock)

	def test_rollback_runs_when_setup_fails(self):
		marker = f"live-safety-{frappe.generate_hash(length=8)}"

		class FailingSetUp(LiveSafeTestCase):
			def setUp(inner):
				super().setUp()
				frappe.get_doc({"doctype": "ToDo", "description": marker}).insert(ignore_permissions=True)
				raise RuntimeError("setUp failed after a write")

			def runTest(inner):
				pass

		result = unittest.TestResult()
		FailingSetUp().run(result)
		self.assertEqual(len(result.errors), 1)
		self.assertFalse(frappe.db.exists("ToDo", {"description": marker}))


class TestAlertIsolation(LiveSafeTestCase):
	"""A module that does not opt in can neither deliver alerts nor touch a real alert key."""

	def test_alert_delivery_is_mocked(self):
		self.assertIsInstance(self.alerts_mock, MagicMock)
		self.assertIs(alerts.account_status_changed, self.alerts_mock)
		self.assertIsInstance(frappe.sendmail, MagicMock)

	def test_alert_keys_are_test_namespaced_and_cleared(self):
		key = alerts.alert_key("isolation-check", "down")
		self.assertFalse(key.startswith("yourwhatsapp:alert:"))
		frappe.cache.set_value(key, 1)
		self.assertIn(key, self._alert_keys)
		self.tearDown()
		self.assertIsNone(frappe.cache.get_value(key, use_local_cache=False))
		self.setUp()
