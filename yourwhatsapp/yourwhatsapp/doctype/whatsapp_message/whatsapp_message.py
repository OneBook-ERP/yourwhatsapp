import frappe
from frappe.model.document import Document

from yourwhatsapp.yourwhatsapp.routing import pick_account


class WhatsAppMessage(Document):
	def before_insert(self):
		if self.type == "Outgoing":
			self.message_type = "Template" if self.use_template else "Manual"
			if not self.whatsapp_account:
				self.whatsapp_account = pick_account()
			self.status = "Queued"
		else:
			self.status = "Received"

	def after_insert(self):
		if self.type == "Outgoing":
			frappe.enqueue(
				"yourwhatsapp.yourwhatsapp.relay_client.send_message",
				queue="long",
				message_name=self.name,
				enqueue_after_commit=True,
			)
