import os
import re

import frappe

# `<account>-qr.png`, or the auto-suffixed `<account>-qr<6 hex>.png` a racing QR rotation got.
QR_FILE_NAME = re.compile(r"(.+)-qr(?:[0-9a-f]{6})?\.png")


def execute():
	"""Delete public QR images on disk that no File record points at.

	QR files were public before the 2026-09-18 fix, and a QR rotation that
	raced the delete got an auto-suffixed name. The File records went away,
	but those bytes stayed served under /files/.
	"""
	accounts = set(frappe.get_all("WhatsApp Account", pluck="name"))
	accounts |= set(frappe.get_all("WhatsApp Message", distinct=True, pluck="whatsapp_account"))
	referenced = set(frappe.get_all("File", pluck="file_url"))
	deleted = _delete_orphans(frappe.get_site_path("public", "files"), referenced, accounts)
	print(f"Deleted {deleted} orphan public QR image(s)")


def _delete_orphans(public_dir: str, referenced: set[str], accounts: set[str]) -> int:
	"""Remove `<account>-qr*.png` files in public_dir whose /files/ URL is not in referenced."""
	deleted = 0
	for name in os.listdir(public_dir):
		match = QR_FILE_NAME.fullmatch(name)
		if not match or match.group(1) not in accounts or f"/files/{name}" in referenced:
			continue
		os.remove(os.path.join(public_dir, name))
		deleted += 1
	return deleted
