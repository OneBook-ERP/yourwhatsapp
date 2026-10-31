import frappe
from frappe.tests import IntegrationTestCase

from yourwhatsapp.patches.v1_0 import approve_existing_whatsapp_templates


class TestWhatsAppTemplates(IntegrationTestCase):
	def tearDown(self):
		frappe.db.rollback()

	def _template(self, name, **fields):
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Templates",
				"template_name": name,
				"template": "Hello {{1}}",
				"parameters": [{"placeholder": 1, "field": "name"}],
				**fields,
			}
		).insert(ignore_permissions=True)

	def _crm_picker(self, doctype):
		# The exact query of CRM's WhatsappTemplateSelectorModal.vue.
		return frappe.get_list(
			"WhatsApp Templates",
			fields=["name", "template", "footer"],
			filters={"status": "APPROVED", "for_doctype": ["in", [doctype, ""]]},
			pluck="name",
		)

	def test_new_template_defaults_to_approved(self):
		self.assertEqual(self._template("G2 Default").status, "APPROVED")

	def test_crm_picker_query_returns_templates(self):
		self._template("G2 Any")
		self._template("G2 Lead", for_doctype="CRM Lead")
		self._template("G2 Deal", for_doctype="CRM Deal")
		self._template("G2 Pending", status="PENDING")
		names = self._crm_picker("CRM Lead")
		self.assertIn("G2 Any", names)
		self.assertIn("G2 Lead", names)
		self.assertNotIn("G2 Deal", names)
		self.assertNotIn("G2 Pending", names)

	def test_patch_approves_existing_rows(self):
		self._template("G2 Legacy")
		frappe.db.set_value("WhatsApp Templates", "G2 Legacy", "status", None)
		approve_existing_whatsapp_templates.execute()
		self.assertEqual(frappe.db.get_value("WhatsApp Templates", "G2 Legacy", "status"), "APPROVED")
