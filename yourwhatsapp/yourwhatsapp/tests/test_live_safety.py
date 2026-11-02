import ast
import pathlib
import unittest

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
