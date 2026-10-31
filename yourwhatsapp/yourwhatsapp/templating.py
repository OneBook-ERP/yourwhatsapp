"""Fill a WhatsApp template's {{n}} placeholders from the message's reference document."""

import datetime
import re

import frappe
from frappe import _
from frappe.model import default_fields, no_value_fields, table_fields
from frappe.utils import cstr, format_datetime, formatdate

PLACEHOLDER = re.compile(r"\{\{(\d+)\}\}")
# Never readable into a customer message: secrets, file paths, raw structures.
BLOCKED_FIELDTYPES = {
	*no_value_fields,
	*table_fields,
	"Password",
	"Attach",
	"Attach Image",
	"Code",
	"Signature",
	"Geolocation",
	"JSON",
}
USER_FIELDS = {"full_name", "first_name", "last_name"}
NUMERIC_FIELDTYPES = {"Int", "Float", "Currency", "Percent"}


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
		error = reference and field_error(reference.doctype, row.field)
		if error:
			problems.append(_("{0} ({1}) but {2}").format(_tags([n]), row.field, error))
			continue
		value = _read(reference, row.field) if reference else None
		if _has_value(value):
			values[n - 1] = value
		elif _has_value(row.default_value):
			values[n - 1] = row.default_value
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
	error = _usable_field_error(doctype, head)
	if error or not target:
		return error
	linked_doctype = _link_target(doctype, head)
	if not linked_doctype:
		return _("{0}.{1} is not a Link field").format(doctype, head)
	if "." in target:
		return _("{0} follows more than one link").format(field)
	return _usable_field_error(linked_doctype, target)


def _reference_doc(doctype, name):
	if not doctype or not name or not frappe.db.exists(doctype, name):
		return None
	return frappe.get_doc(doctype, name)


def _read(doc, field):
	"""The field's value as text, or None; `field` has passed field_error."""
	head, _sep, target = field.partition(".")
	value = doc.get(head)
	if not target or not _has_value(value):
		return _to_text(value, frappe.get_meta(doc.doctype).get_field(head), doc)
	linked = frappe.get_doc(_link_target(doc.doctype, head), value)
	return _to_text(linked.get(target), linked.meta.get_field(target), linked)


def _usable_field_error(doctype, fieldname) -> str | None:
	if fieldname in default_fields:
		return None
	df = frappe.get_meta(doctype).get_field(fieldname)
	if not df:
		return _("{0} has no field {1}").format(doctype, fieldname)
	if df.fieldtype in BLOCKED_FIELDTYPES or (doctype == "User" and fieldname not in USER_FIELDS):
		return _("field {0} can't be used in templates").format(fieldname)
	return None


def _link_target(doctype, fieldname):
	df = frappe.get_meta(doctype).get_field(fieldname)
	return df.options if df and df.fieldtype == "Link" else None


def _has_value(value) -> bool:
	return value is not None and cstr(value).strip() != ""


def _to_text(value, df=None, doc=None) -> str | None:
	if not _has_value(value):
		return None
	if df and df.fieldtype in NUMERIC_FIELDTYPES:
		return frappe.format_value(value, df, doc)
	if isinstance(value, datetime.datetime):
		return format_datetime(value)
	if isinstance(value, datetime.date):
		return formatdate(value)
	return cstr(value)


def _tags(numbers) -> str:
	return ", ".join("{{" + str(n) + "}}" for n in numbers)
