import glob
import os

import frappe


def execute():
	"""Delete public QR images on disk that no File record points at.

	QR files were public before the 2026-09-18 fix, and a QR rotation that
	raced the delete got an auto-suffixed name (`<account>-qr<hash>.png`). The
	File records went away, but those bytes stayed served under /files/.
	"""
	public_dir = frappe.get_site_path("public", "files")
	referenced = set(frappe.get_all("File", filters={"is_private": 0}, pluck="file_url"))
	deleted = 0
	for path in glob.glob(os.path.join(public_dir, "*-qr*.png")):
		if f"/files/{os.path.basename(path)}" in referenced:
			continue
		os.remove(path)
		deleted += 1
	print(f"Deleted {deleted} orphan public QR image(s)")
