## Yourwhatsapp

Self-hosted WhatsApp sending/receiving layer for Frappe CRM, backed by an unofficial
client behind a small self-hosted relay (a Go relay on `whatsmeow`, replacing the earlier
Node/`whatsapp-web.js` one). Built for a business whose category
is restricted under Meta's policy, so the official WhatsApp Business API isn't available.

It supplies `WhatsApp Message`, `WhatsApp Account`, `WhatsApp Settings` and
`WhatsApp Templates` with the same names and shape Frappe CRM already integrates
against, so CRM's WhatsApp UI works unmodified — messages just go through your own
relay instead of Meta's Cloud API.

**This is an internal ops tool.** It is not approved by, affiliated with, or endorsed by
Meta/WhatsApp, and no self-hosted setup of this kind is "ban-proof". The anti-ban work
tracked in `ROADMAP.md` reduces risk; it does not remove it. Don't describe it otherwise.

### How it fits together

```
Frappe CRM ──> crm.api.whatsapp.* ──> doctypes (this app) ──> relay_client.py
                                                                   │ HTTP + x-api-key
                                                                   ▼
                                              Go relay (one whatsmeow client per
                                              WhatsApp Account) ──> WhatsApp
```

Outgoing messages are routed across accounts (round robin or priority), respect a daily
cap per number, and fail over once to another account. Incoming messages, delivery
receipts and QR codes come back through whitelisted endpoints in `api.py`.

### Requirements

- Frappe v16 + Frappe CRM
- Go 1.27+ to build the relay (or use the Docker image); no browser needed
- One or more real, SIM-registered WhatsApp numbers (not VOIP)

### Installation

```bash
cd $PATH_TO_YOUR_BENCH
bench get-app https://github.com/OneBook-ERP/yourwhatsapp --branch version-16
bench --site $SITE install-app yourwhatsapp
```

Then build and start the relay (`cd yourwhatsapp/yourwhatsapp/relay && make build`, env vars
and routes in `yourwhatsapp/yourwhatsapp/relay/README.md`), fill in **WhatsApp Settings**
(`relay_base_url`, `relay_api_key`), create a **WhatsApp Account** and click
**Connect / Generate QR** to pair a number by scanning from WhatsApp → Linked Devices.

### Production

Production runs the relay as its own container next to the Frappe stack. See
`DEPLOYMENT.md` for the image build, compose override, backups and warm-up advice. The
relay keeps device keys in one SQLite file (`relay.db`): back it up. The Docker files are
untested until the first real deploy. Remaining blockers are tracked in `ROADMAP.md`
(Phase 1.6).

### Status

Working and tested against real numbers: pairing, text and media in both directions,
routing, failover, delivery/read receipts, CRM linking of conversations to Leads/Deals.

Not built yet: trigger-based automated messaging, sender health scoring, the operations
dashboard. See `ROADMAP.md` — that file is the source of truth for what's next.

### Contributing

This app uses `pre-commit` for code formatting and linting. Please [install pre-commit](https://pre-commit.com/#installation) and enable it for this repository:

```bash
cd apps/yourwhatsapp
pre-commit install
```

Pre-commit is configured to use the following tools for checking and formatting your code:

- ruff
- eslint
- prettier
- pyupgrade

### CI

This app can use GitHub Actions for CI. The following workflows are configured:

- CI: Installs this app and runs unit tests on every push to `develop` branch.
- Linters: Runs [Frappe Semgrep Rules](https://github.com/frappe/semgrep-rules) and [pip-audit](https://pypi.org/project/pip-audit/) on every pull request.

### License

mit
