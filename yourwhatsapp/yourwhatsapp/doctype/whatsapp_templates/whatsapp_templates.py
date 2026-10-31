import frappe
from frappe import _
from frappe.model.document import Document

from yourwhatsapp.yourwhatsapp.templating import field_error, placeholders


class WhatsAppTemplates(Document):
	def validate(self):
		numbers = placeholders(self.template)
		if numbers != list(range(1, len(numbers) + 1)):
			frappe.throw(_("Number the placeholders {{1}}, {{2}}, ... without gaps."), frappe.ValidationError)
		self._validate_parameter_rows(numbers)

	def _validate_parameter_rows(self, numbers):
		seen = set()
		for row in self.parameters:
			if row.placeholder in seen:
				frappe.throw(_("Placeholder {0} has more than one parameter row.").format(row.placeholder))
			seen.add(row.placeholder)
			error = self.for_doctype and field_error(self.for_doctype, row.field)
			if error:
				frappe.throw(_("Parameter row for placeholder {0}: {1}.").format(row.placeholder, error))
		missing = [n for n in numbers if n not in seen]
		if missing:
			frappe.throw(
				_("Add a parameter row for placeholder(s) {0}.").format(", ".join(map(str, missing))),
				frappe.ValidationError,
			)
