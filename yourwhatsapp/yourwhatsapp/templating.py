"""Fill a WhatsApp template's {{n}} placeholders from the message's reference document."""

import datetime
import re

import frappe
from frappe import _
from frappe.model import default_fields
from frappe.utils import cstr, format_datetime, formatdate

PLACEHOLDER = re.compile(r"\{\{(\d+)\}\}")


class UnresolvedPlaceholders(frappe.ValidationError):
	pass


def placeholders(text) -> list[int]:
	return sorted({int(n) for n in PLACEHOLDER.findall(text or "")})


def render(text, values) -> str:
	"""Substitute {{n}} with values[n-1]; raise if any placeholder has no value."""
	missing = []

	def substitute(match):
		n = int(match.group(1))
		if 1 <= n <= len(values) and values[n - 1] is not None:
			return cstr(values[n - 1])
		missing.append(n)
		return match.group(0)

	text = PLACEHOLDER.sub(substitute, text or "")
	if missing:
		raise UnresolvedPlaceholders(_("Unresolved template placeholders: {0}").format(_tags(missing)))
	return text


def resolve(template_name, reference_doctype, reference_name) -> list[str]:
	"""Values for {{1}}..{{max}}, read with system permissions (it's a system send)."""
	template = frappe.get_doc("WhatsApp Templates", template_name)
	numbers = placeholders(template.template)
	rows = {row.placeholder: row for row in template.parameters}
	reference = _reference_doc(reference_doctype, reference_name)
	values = [""] * (max(numbers) if numbers else 0)
	problems = []
	for n in numbers:
		row = rows.get(n)
		if not row:
			problems.append(_("{0}, which has no parameter row (add parameter rows)").format(_tags([n])))
			continue
		value = _read(reference, row.field) if reference else None
		value = value if _has_value(value) else row.default_value
		if _has_value(value):
			values[n - 1] = _to_text(value)
		elif not reference:
			problems.append(_("{0} ({1}) but the message has no reference document").format(_tags([n]), row.field))
		else:
			problems.append(
				_("{0} ({1}) but {2} {3} has no value").format(
					_tags([n]), row.field, reference.doctype, reference.name
				)
			)
	if problems:
		frappe.throw(
			_("Template '{0}' needs {1}.").format(template_name, "; ".join(problems)), frappe.ValidationError
		)
	return values


def field_error(doctype, field) -> str | None:
	"""Why `field` (fieldname or link_field.target_field) can't be read on `doctype`, or None."""
	head, _sep, target = field.partition(".")
	if not _field_exists(doctype, head):
		return _("{0} has no field {1}").format(doctype, head)
	if not target:
		return None
	linked_doctype = _link_target(doctype, head)
	if not linked_doctype:
		return _("{0}.{1} is not a Link field").format(doctype, head)
	if "." in target:
		return _("{0} follows more than one link").format(field)
	if not _field_exists(linked_doctype, target):
		return _("{0} has no field {1}").format(linked_doctype, target)
	return None


def _reference_doc(doctype, name):
	if not doctype or not name or not frappe.db.exists(doctype, name):
		return None
	return frappe.get_doc(doctype, name)


def _read(doc, field):
	if field_error(doc.doctype, field):
		return None
	head, _sep, target = field.partition(".")
	value = doc.get(head)
	if not target or not value:
		return value
	return frappe.db.get_value(_link_target(doc.doctype, head), value, target)


def _field_exists(doctype, fieldname) -> bool:
	return fieldname in default_fields or bool(frappe.get_meta(doctype).get_field(fieldname))


def _link_target(doctype, fieldname):
	df = frappe.get_meta(doctype).get_field(fieldname)
	return df.options if df and df.fieldtype == "Link" else None


def _has_value(value) -> bool:
	return value is not None and cstr(value).strip() != ""


def _to_text(value) -> str:
	if isinstance(value, datetime.datetime):
		return format_datetime(value)
	if isinstance(value, datetime.date):
		return formatdate(value)
	return cstr(value)


def _tags(numbers) -> str:
	return ", ".join("{{" + str(n) + "}}" for n in numbers)
