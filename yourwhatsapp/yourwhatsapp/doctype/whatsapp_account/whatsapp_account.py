import requests
import frappe
from frappe.model.document import Document
from frappe.utils import cint, random_string

from yourwhatsapp.yourwhatsapp.relay_client import relay_settings

TIMEOUT = 15


class WhatsAppAccount(Document):
	def before_insert(self):
		if not self.session_id:
			self.session_id = f"acc-{random_string(10).lower()}"

	@frappe.whitelist()
	def connect(self):
		if "WhatsApp Admin" not in frappe.get_roles() and "System Manager" not in frappe.get_roles():
			frappe.throw("Not permitted to connect a WhatsApp Account.", frappe.PermissionError)

		base_url, api_key = relay_settings()
		response = requests.post(
			f"{base_url}/session/{self.session_id}/start",
			headers={"x-api-key": api_key},
			timeout=TIMEOUT,
		)
		response.raise_for_status()
		return response.json()

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
