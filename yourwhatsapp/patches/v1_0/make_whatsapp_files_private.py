import frappe


def execute():
	"""Make existing WhatsApp media private and drop leftover public QR images.

	Inbound media was saved with Frappe's default is_private=0, so every file a
	customer sent was readable by anyone with the URL. QR images were public too,
	under a filename derived from the account name — guessable while a number was
	pairing, and a scanned QR links the scanner's own device to that number.
	"""
	_make_inbound_media_private()
	_delete_qr_files()


def _make_inbound_media_private():
	files = frappe.get_all(
		"File",
		filters={"attached_to_doctype": "WhatsApp Message", "is_private": 0},
		pluck="name",
	)
	for name in files:
		file_doc = frappe.get_doc("File", name)
		old_url = file_doc.file_url
		file_doc.is_private = 1
		try:
			# File.handle_is_private_changed() moves the file on disk and rewrites file_url.
			file_doc.save(ignore_permissions=True)
		except Exception:
			# Missing bytes on disk shouldn't strand the remaining files as public.
			frappe.log_error(frappe.get_traceback(), f"Could not make WhatsApp file private: {name}")
			continue

		if file_doc.file_url != old_url:
			frappe.db.set_value(
				"WhatsApp Message",
				{"attach": old_url},
				"attach",
				file_doc.file_url,
				update_modified=False,
			)


def _delete_qr_files():
	# QR codes are short-lived and regenerated on demand, so there's nothing worth
	# migrating — a stale public one is only a liability.
	for name in frappe.get_all("File", filters={"attached_to_doctype": "WhatsApp Account"}, pluck="name"):
		frappe.delete_doc("File", name, ignore_permissions=True, delete_permanently=True)

	frappe.db.set_value("WhatsApp Account", {"qr_code": ["is", "set"]}, "qr_code", None, update_modified=False)
