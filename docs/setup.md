# Setting up yourwhatsapp in a new environment

yourwhatsapp has two parts:

- **The Frappe app** (`yourwhatsapp`). It supplies the `WhatsApp Message`, `WhatsApp Account`, `WhatsApp Settings` and `WhatsApp Templates` doctypes that Frappe CRM already uses, so CRM's WhatsApp tab works unmodified.
- **The relay**, a small Go program (whatsmeow). It holds one WhatsApp connection per linked number and talks to Frappe over HTTP.

Two ways to run it:

- **A. Production with Docker (frappe_docker).** Full detail: [`DEPLOYMENT.md`](../DEPLOYMENT.md).
- **B. Dev or bare-metal bench.** This is how the dev bench runs.

This is an unofficial WhatsApp client. Everything here *reduces* the risk of numbers being banned; nothing makes them ban-proof.

---

## A. Production with Docker

**You need:** a Docker host you control (frappe_docker with MariaDB and Redis). Managed hosts that can't run an extra container can't run the relay; see `DEPLOYMENT.md` §9.

### 1. Build the Frappe image with the apps

In frappe_docker, list the apps in `apps.json`. Use the branch you deploy.

```json
[
  {"url": "https://github.com/frappe/erpnext", "branch": "version-16"},
  {"url": "https://github.com/frappe/crm", "branch": "main"},
  {"url": "https://github.com/OneBook-ERP/yourwhatsapp", "branch": "version-16"}
]
```

```bash
export APPS_JSON_BASE64=$(base64 -w 0 apps.json)
docker build --build-arg=APPS_JSON_BASE64=$APPS_JSON_BASE64 ... -t <your-frappe-image> .
```

Check the exact build command and the app branches against your frappe_docker version.

### 2. Build the relay image

```bash
cd apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay
docker build --build-arg VERSION=<tag> -t ghcr.io/<org>/yourwhatsapp-relay:<tag> .
docker push ghcr.io/<org>/yourwhatsapp-relay:<tag>
```

### 3. Start the stack with the relay

```bash
# Keep this token: you type it into WhatsApp Settings in step 5.
export RELAY_BOOTSTRAP_TOKEN=$(python3 -c "import secrets;print(secrets.token_urlsafe(32))")

docker compose -f compose.yaml \
  -f overrides/compose.mariadb.yaml -f overrides/compose.redis.yaml \
  -f <path-to>/relay/compose.whatsapp.yaml up -d
```

- The relay has **no published port**. Only Frappe reaches it, at `http://whatsapp-relay:8800`.
- Its volume (`whatsapp-relay-data`) holds every number's WhatsApp login. **Back it up** (`DEPLOYMENT.md` §6). Losing it means relinking every number.

### 4. Create the site

Follow the usual frappe_docker steps: `bench new-site`, then `bench --site <site> install-app erpnext crm yourwhatsapp`.

### 5. Connect the relay (browser only)

In **WhatsApp Settings**, as System Manager:

1. **Relay Base URL:** `http://whatsapp-relay:8800`
2. **Callback URL:** Frappe as the relay sees it, e.g. `http://frontend:8080`
3. **Relay Bootstrap Token:** the token from step 3
4. Click **Connect Relay**. Frappe generates the key and claims the relay.
5. Click **Test Connection**. It should show **Connected**.

Use **Rotate Key** to change the key later and **Update Callback URL** if Frappe's address changes. If the panel ever says the keys are rejected, the recovery steps are in `DEPLOYMENT.md` §4.

### 6. Link numbers

1. Create a **WhatsApp Account** per number. Choose **Pairing Method**: QR Code, or Phone Number (8-character code).
2. Click **Connect**:
   - **QR Code:** on the phone, open WhatsApp → Linked Devices → Link a device, and scan the QR.
   - **Phone Number:** on the phone, open WhatsApp → Linked Devices → Link a device → Link with phone number instead, and type the code.
3. The account turns **Active**. The Settings default number fills itself in.

### 7. Before going live

- **Alerts:**
  - Give the people who should hear about outages the **System Manager** or **WhatsApp Admin** role. They get a bell notification when a number goes down or comes back.
  - Add an outgoing **Email Account** in Frappe so alerts are emailed too.
- **Warm-up:** start new numbers at **daily limit 20** and raise slowly. See [`warm-up.md`](warm-up.md).
- **Templates:** in each WhatsApp Template's **Parameters** table, map every `{{1}}`, `{{2}}`… to a field of the document the message is about. A template with an unmapped placeholder is refused when sent.
- **Large incoming files:** if customers send big videos or PDFs, raise the limits together (`DEPLOYMENT.md`, "Inbound media size limits").
- **Permissions:** read [`rbac-audit.md`](rbac-audit.md) and decide on the recommendations.

---

## B. Dev or bare-metal bench

**You need:**
- a Frappe v16 bench with **MariaDB** (the claim screen uses MariaDB locks), Redis, and the `crm` app (and `erpnext` if you use it);
- **Go 1.27.3+** to build the relay.

### 1. Install the app

```bash
cd <bench>
bench get-app https://github.com/OneBook-ERP/yourwhatsapp --branch <branch>
bench --site <site> install-app yourwhatsapp
bench --site <site> migrate
bench --site <site> enable-scheduler      # status checks, retries and ban recovery need it
```

### 2. Build the relay

```bash
cd <bench>/apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay
make build        # -> bin/relay (static, CGO_ENABLED=0)
make test         # optional
```

### 3. Run the relay with bench

Add to `<bench>/Procfile`:

```
relay: apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay/bin/relay
```

Add to `<bench>/.env` (honcho loads it for every Procfile entry):

```bash
RELAY_PORT=8800
RELAY_DATA_DIR=<bench>/apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay/data
FRAPPE_BASE_URL=http://127.0.0.1:8001      # your bench web port; use 127.0.0.1, not *.localhost
# Choose ONE way to set the key:
RELAY_API_KEY=<long random string>         # "env mode": you type the same key into Settings
# ...or leave RELAY_API_KEY out, set RELAY_BOOTSTRAP_TOKEN=<token>, and use Connect Relay.
```

Generate random values with `python3 -c "import secrets;print(secrets.token_urlsafe(32))"`.

Then run `bench start`.

### 4. Connect the relay

In **WhatsApp Settings**:

1. **Relay Base URL:** `http://localhost:8800`.
2. Set the key, depending on the mode you chose in step 3:
   - **Env mode:** type the `RELAY_API_KEY` value into **Relay API Key**. The panel tells you this when it detects env mode.
   - **Claim mode:** set **Callback URL** to `http://127.0.0.1:8001`, fill in the bootstrap token, then click **Connect Relay**.
3. Click **Test Connection**. It should show **Connected**.

### 5. Link numbers

Same as A step 6. Then go through A step 7.

---

## Checking it works

- **Test Connection** in WhatsApp Settings shows **Connected** and "*n* / *n*" numbers.
- From a CRM Lead's WhatsApp tab, send a message to a number you own. It should go Queued → Sent → Delivered.
- Reply from the phone. The reply appears in the same Lead's WhatsApp tab.

## Common problems

| Symptom | Cause / fix |
|---|---|
| Relay won't start: "linked numbers but no claim" | Its data folder already has linked numbers and there is no key. Set `RELAY_API_KEY`, or `RELAY_BOOTSTRAP_TOKEN` and claim again. This guard stops a stranger claiming a relay that already holds live numbers. |
| Test Connection: **Unreachable** | Wrong Relay Base URL, or the relay isn't running. |
| Test Connection: **Key rejected** | The key in Settings doesn't match the relay. In env mode, retype `RELAY_API_KEY`. |
| Callbacks fail / relay can't reach Frappe | Callback URL or `FRAPPE_BASE_URL` uses a `.localhost` name. Use `http://127.0.0.1:<port>`. |
| Numbers show Disconnected after a restart | The relay retries on its own with backoff. If one stays down, click **Connect** on the account. A bell alert tells admins. |
| No alerts arrive | No user with System Manager or WhatsApp Admin, or no outgoing Email Account (bell only). |
| `EADDRINUSE :9001` on `bench start` | A previous `bench start` was stopped with Ctrl+Z and is still running. Stop it with Ctrl+C next time. |
| CRM hides the WhatsApp tab | No **Active** number. Link one; the default fills itself in. |

## Rules that save you later

- **Never run two relays** on the same data folder or the same linked number. WhatsApp kicks one off.
- **Stop the relay gracefully** (Ctrl+C, `docker compose stop`), never `kill -9`. Repeated hard kills can get a device unlinked.
- **Back up the relay data folder or volume.** It is the WhatsApp login for every number; treat it like a password.
- **The scheduler must be on.** The 5-minute status check, message retries and ban recovery all run from it.
