from yourwhatsapp.yourwhatsapp.routing import fill_default_accounts


def execute():
	"""CRM shows WhatsApp only while the default outgoing account is Active; fill any empty, dangling or inactive default."""
	fill_default_accounts()
