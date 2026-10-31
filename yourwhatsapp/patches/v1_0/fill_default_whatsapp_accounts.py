import frappe

from yourwhatsapp.yourwhatsapp.routing import fill_default_accounts


def execute():
	"""CRM shows WhatsApp only when a default account is set; fill any empty or dangling default."""
	account = frappe.db.get_value(
		"WhatsApp Account", {"status": "Active"}, "name", order_by="priority asc, creation asc"
	)
	if account:
		fill_default_accounts(account)
