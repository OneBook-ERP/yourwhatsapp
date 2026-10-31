import json
from unittest.mock import patch

import frappe
from frappe.tests import IntegrationTestCase
from frappe.utils import formatdate

from yourwhatsapp.yourwhatsapp import relay_client, routing
from yourwhatsapp.yourwhatsapp.templating import placeholders, render, resolve


class TestTemplateParameters(IntegrationTestCase):
	def setUp(self):
		# Never route to (or enqueue a send on) the live site's accounts.
		self.enqueue = patch("frappe.enqueue").start()
		patch.object(routing, "get_eligible_accounts", return_value=[]).start()
		self.addCleanup(patch.stopall)

	def tearDown(self):
		frappe.db.rollback()

	def _template(self, name, body, rows=(), **fields):
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Templates",
				"template_name": name,
				"template": body,
				"parameters": [
					{"placeholder": placeholder, "field": field, "default_value": default}
					for placeholder, field, default in rows
				],
				**fields,
			}
		).insert(ignore_permissions=True)

	def _lead(self, **fields):
		return frappe.get_doc({"doctype": "CRM Lead", "first_name": "Ana", **fields}).insert(
			ignore_permissions=True
		)

	def _outgoing(self, template, to="15557770001", **fields):
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Message",
				"type": "Outgoing",
				"to": to,
				"content_type": "text",
				"message": "Template message",
				"use_template": 1,
				"template": template,
				**fields,
			}
		).insert(ignore_permissions=True)

	def test_placeholders_and_render(self):
		self.assertEqual(placeholders("{{2}} and {{1}} and {{2}}"), [1, 2])
		self.assertEqual(render("Hi {{1}}, {{2}}", ["Ana", "Acme"]), "Hi Ana, Acme")

	def test_resolve_lead_fields(self):
		self._template("TP Lead", "Hi {{1}} from {{2}}", [(1, "first_name", None), (2, "organization", None)])
		lead = self._lead(organization="Acme")
		self.assertEqual(resolve("TP Lead", "CRM Lead", lead.name), ["Ana", "Acme"])

	def test_resolve_dotted_link_from_deal(self):
		self._template("TP Deal", "Hi {{1}}", [(1, "lead.first_name", None)], for_doctype="CRM Deal")
		lead = self._lead(first_name="Bruno")
		deal = frappe.get_doc({"doctype": "CRM Deal", "lead": lead.name}).insert(ignore_permissions=True)
		self.assertEqual(resolve("TP Deal", "CRM Deal", deal.name), ["Bruno"])

	def test_resolve_customer_and_sales_order(self):
		customer = frappe.get_doc(
			{"doctype": "Customer", "customer_name": "TP Customer", "customer_type": "Individual"}
		).insert(ignore_permissions=True)
		self._template("TP Customer", "Dear {{1}}", [(1, "customer_name", None)], for_doctype="Customer")
		self.assertEqual(resolve("TP Customer", "Customer", customer.name), ["TP Customer"])

		# A real Sales Order needs items, prices and stock setup; the row alone is
		# enough to read from, and the transaction rolls it back.
		order = frappe.get_doc(
			{
				"doctype": "Sales Order",
				"name": "TP-SO-0001",
				"customer": customer.name,
				"transaction_date": "2026-11-05",
				"company": frappe.db.get_value("Company", {}, "name"),
			}
		)
		order.db_insert()
		self._template(
			"TP Order",
			"Order {{1}} for {{2}}",
			[(1, "name", None), (2, "customer.customer_name", None)],
			for_doctype="Sales Order",
		)
		self.assertEqual(resolve("TP Order", "Sales Order", order.name), ["TP-SO-0001", "TP Customer"])

	def test_default_value_used(self):
		self._template("TP Default", "Hi {{1}} at {{2}}", [(1, "first_name", None), (2, "organization", "your company")])
		lead = self._lead()
		self.assertEqual(resolve("TP Default", "CRM Lead", lead.name), ["Ana", "your company"])

	def test_missing_value_blocks(self):
		self._template("TP Missing", "Hi {{1}} at {{2}}", [(1, "first_name", None), (2, "organization", None)])
		lead = self._lead()
		with self.assertRaises(frappe.ValidationError) as ctx:
			resolve("TP Missing", "CRM Lead", lead.name)
		self.assertIn(
			f"Template 'TP Missing' needs {{{{2}}}} (organization) but CRM Lead {lead.name} has no value",
			str(ctx.exception),
		)

	def test_missing_row_blocks(self):
		# A template saved before parameter rows existed (validate now refuses it).
		template = self._template("TP No Row", "Hi")
		frappe.db.set_value("WhatsApp Templates", template.name, "template", "Hi {{1}}")
		lead = self._lead()
		with self.assertRaises(frappe.ValidationError) as ctx:
			resolve("TP No Row", "CRM Lead", lead.name)
		self.assertIn("{{1}}", str(ctx.exception))
		self.assertIn("add parameter rows", str(ctx.exception))

	def test_no_reference_blocks(self):
		self._template("TP No Ref", "Hi {{1}}", [(1, "first_name", None)])
		with self.assertRaises(frappe.ValidationError) as ctx:
			resolve("TP No Ref", None, None)
		self.assertIn("no reference document", str(ctx.exception))

	def test_no_reference_uses_number_match(self):
		self._template("TP Match", "Hi {{1}}", [(1, "first_name", None)])
		lead = self._lead(first_name="Carla", mobile_no="+1 415 555 0137")
		msg = self._outgoing("TP Match", to="+1 (415) 555-0137")
		self.assertEqual((msg.reference_doctype, msg.reference_name), ("CRM Lead", lead.name))
		self.assertEqual(json.loads(msg.template_parameters), ["Carla"])

	def test_unresolvable_blocks_insert_no_enqueue(self):
		self._template("TP Blocked", "Hi {{1}}", [(1, "organization", None)])
		lead = self._lead()
		with self.assertRaises(frappe.ValidationError):
			self._outgoing("TP Blocked", reference_doctype="CRM Lead", reference_name=lead.name)
		self.enqueue.assert_not_called()
		self.assertFalse(frappe.db.exists("WhatsApp Message", {"template": "TP Blocked"}))

	def test_template_validate_requires_rows_and_valid_fields(self):
		invalid = {
			"no row": ("Hi {{1}} {{2}}", [(1, "first_name", None)], None),
			"gap": ("Hi {{1}} {{3}}", [(1, "first_name", None), (3, "first_name", None)], None),
			"duplicate row": ("Hi {{1}}", [(1, "first_name", None), (1, "last_name", None)], None),
			"unknown field": ("Hi {{1}}", [(1, "no_such_field", None)], "CRM Lead"),
			"dotted non-link": ("Hi {{1}}", [(1, "first_name.name", None)], "CRM Lead"),
			"dotted unknown target": ("Hi {{1}}", [(1, "lead.no_such_field", None)], "CRM Deal"),
			"two levels": ("Hi {{1}}", [(1, "lead.organization.name", None)], "CRM Deal"),
		}
		for case, (body, rows, for_doctype) in invalid.items():
			with self.subTest(case), self.assertRaises(frappe.ValidationError):
				self._template(f"TP Invalid {case}", body, rows, for_doctype=for_doctype)

		self._template("TP Valid", "Hi {{1}} {{2}}", [(1, "lead.first_name", None), (2, "name", None)], for_doctype="CRM Deal")
		self._template("TP Valid Any", "Hi {{1}}", [(1, "anything", None)])

	def test_render_final_check_fails_message(self):
		self._template("TP Final", "Hi {{1}} at {{2}}", [(1, "first_name", None), (2, "organization", None)])
		account = frappe.get_doc(
			{"doctype": "WhatsApp Account", "account_name": "TP Sender", "phone_number": "15550009999", "status": "Active"}
		).insert(ignore_permissions=True)
		# Parameters already set skip resolution, as when a caller supplies them.
		msg = self._outgoing("TP Final", whatsapp_account=account.name, template_parameters=json.dumps(["Ana"]))
		with patch("requests.post") as post:
			relay_client.send_message(msg.name)
		post.assert_not_called()
		row = frappe.db.get_value("WhatsApp Message", msg.name, ["status", "error_reason"], as_dict=True)
		self.assertEqual((row.status, row.error_reason), ("Failed", "unresolved template placeholders"))

	def test_crm_send_whatsapp_template_end_to_end(self):
		from crm.api.whatsapp import send_whatsapp_template

		self._template("TP CRM", "Hi {{1}} from {{2}}", [(1, "first_name", None), (2, "organization", None)])
		lead = self._lead(first_name="Dora", organization="Dora Farms", mobile_no="+1 415 555 0138")
		with patch("requests.post") as post:
			name = send_whatsapp_template("CRM Lead", lead.name, "TP CRM", "+1 415 555 0138")
		post.assert_not_called()
		params = frappe.db.get_value("WhatsApp Message", name, "template_parameters")
		self.assertEqual(json.loads(params), ["Dora", "Dora Farms"])

	def test_date_formatting(self):
		self._template("TP Date", "Closing on {{1}}", [(1, "expected_closure_date", None)], for_doctype="CRM Deal")
		deal = frappe.get_doc(
			{"doctype": "CRM Deal", "lead": self._lead().name, "expected_closure_date": "2026-11-05"}
		).insert(ignore_permissions=True)
		value = resolve("TP Date", "CRM Deal", deal.name)[0]
		self.assertEqual(value, formatdate("2026-11-05"))
		self.assertNotEqual(value, "2026-11-05")
