import frappe
from frappe.model.document import Document
from frappe.utils import cint, random_string


class WhatsAppAccount(Document):
	def before_insert(self):
		if not self.session_id:
			self.session_id = f"acc-{random_string(10).lower()}"

	def validate(self):
		self._enforce_single_default("is_default_incoming")
		self._enforce_single_default("is_default_outgoing")

	def _enforce_single_default(self, fieldname):
		if not cint(self.get(fieldname)):
			return
		others = frappe.get_all(
			"WhatsApp Account",
			filters={fieldname: 1, "name": ["!=", self.name]},
		)
		for other in others:
			frappe.db.set_value("WhatsApp Account", other.name, fieldname, 0)
