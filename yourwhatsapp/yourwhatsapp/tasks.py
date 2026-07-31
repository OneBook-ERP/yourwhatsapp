import frappe


def reset_daily_message_counts():
	frappe.db.set_value("WhatsApp Account", {}, "daily_message_count", 0, update_modified=False)
