import frappe


def execute():
	"""CRM's template picker filters on status=APPROVED; templates created before the field existed have none."""
	frappe.db.set_value(
		"WhatsApp Templates", {"status": ["is", "not set"]}, "status", "APPROVED", update_modified=False
	)
