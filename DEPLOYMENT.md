# Deploying yourwhatsapp (Docker)

Production runs two things: your Frappe stack (built from [frappe_docker](https://github.com/frappe/frappe_docker)) with the `yourwhatsapp` app installed, and the Go relay as its own container next to it. This is an internal ops tool, not approved by Meta/WhatsApp. The Docker files were written without Docker available on the dev host: treat the first deploy as the first test.

## 1. Build the Frappe image with yourwhatsapp

Add the app to the `apps.json` you feed to frappe_docker's image build:

```json
[
  {"url": "https://github.com/frappe/crm", "branch": "version-16"},
  {"url": "https://github.com/OneBook-ERP/yourwhatsapp", "branch": "v1.2.0"}
]
```

Pin yourwhatsapp to a **release tag** (frappe_docker's `branch` field accepts tags), and use the same tag for the relay image in step 2.

```bash
export APPS_JSON_BASE64=$(base64 -w 0 apps.json)
docker build --build-arg=APPS_JSON_BASE64=$APPS_JSON_BASE64 \
  --build-arg=FRAPPE_BRANCH=version-16 \
  --tag=<registry>/frappe-crm:<version> --file=images/layered/Containerfile .
```

Then `bench --site <site> install-app yourwhatsapp` (frappe_docker: `docker compose exec backend bench --site <site> install-app yourwhatsapp`). If your host cannot add containers (e.g. Frappe Cloud), see section 9.

## 2. The relay image: pulled, not built

You don't build the relay on the server. GitHub Actions (`.github/workflows/relay-image.yml`) builds it for every release tag and publishes a **public** image, so the server pulls it like redis or mariadb:

```
ghcr.io/onebook-erp/yourwhatsapp-relay:<tag>      # linux/amd64 + linux/arm64
```

The image is a static Go binary on `gcr.io/distroless/static:nonroot` (about 32 MB), listening on 8800, with its data in `/data`. It reports its tag in `/info` and the Settings relay panel. There is no `latest`: always pin the release tag, the same one as yourwhatsapp in `apps.json`.

Building it yourself is only for forks or testing:

```bash
cd apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay
docker build --build-arg VERSION=<tag> -t yourwhatsapp-relay:<tag> .     # add --platform linux/amd64 on an ARM machine for an x86 server
# then: export RELAY_IMAGE=yourwhatsapp-relay RELAY_VERSION=<tag>
```

## 3. Add the compose override

frappe_docker does **not** discover the relay by itself: the Frappe image only carries the Python app. You add the relay by passing one extra compose file. Copy `yourwhatsapp/yourwhatsapp/relay/compose.whatsapp.yaml` next to your frappe_docker compose files (it reads everything from env, so you don't edit it) and add it to the command you already use:

```bash
# Required in production: only someone holding this token can claim the relay in step 4.
# Keep it, you need it in step 4. /claim is not throttled, so use a long random value.
export RELAY_BOOTSTRAP_TOKEN=$(python3 -c "import secrets;print(secrets.token_urlsafe(32))")
export RELAY_VERSION=v1.2.0          # same release tag as yourwhatsapp in apps.json
docker compose -f compose.yaml -f overrides/compose.mariadb.yaml -f overrides/compose.redis.yaml \
  -f compose.whatsapp.yaml up -d
```

Put `RELAY_BOOTSTRAP_TOKEN` and `RELAY_VERSION` in the same `.env` file as frappe_docker's other variables so every later `docker compose` run picks them up. If you deploy from a generated file (frappe_docker's "gitops" style, `docker compose ... config > prod.yaml`), include `-f compose.whatsapp.yaml` when you generate it.

- `RELAY_BOOTSTRAP_TOKEN` is **required in production**. Without it, whoever reaches the relay port first can claim a fresh relay.
- No `RELAY_API_KEY` is needed: the relay starts **unclaimed** and is claimed from the Desk in step 4. The callback URL and `Host` header come from the claim. `FRAPPE_BASE_URL` / `FRAPPE_SITE_NAME` are used in env mode only (for that, also `export SITE_NAME=<your site>` before `up`), and setting `RELAY_API_KEY` (commented out in the compose file as legacy/env mode) pins the key in env and disables claiming.
- A relay whose volume already holds linked numbers but has no claim (for example after moving from env mode) refuses to start unless `RELAY_BOOTSTRAP_TOKEN` or `RELAY_API_KEY` is set. This stops anyone who can reach the port from claiming live numbers.
- No ports are published. Only containers on the compose network (your Frappe services) reach the relay. Never expose port 8800 to the internet: with the key, anyone can send messages as your numbers.
- `stop_grace_period: 30s` lets the relay close WhatsApp sessions cleanly. Do not shorten it, and avoid `docker kill` on a live relay: repeated abrupt disconnects can get a device unlinked.
- Run exactly one relay per data volume.
- `MEDIA_MAX_BYTES` (default 15 MiB) caps inbound attachments; bigger ones arrive as a text note. The relay logs the effective value at startup (`relay config`, `media_max_bytes`). It must fit through every limit in the chain below.
- The relay runs as uid 65532 (distroless nonroot). A fresh named volume inherits the right ownership from the image; a bind mount or a pre-existing volume needs a host-side `chown -R 65532:65532 <path>` or the relay cannot open `relay.db` and crashes on start.

### Inbound media size limits

An inbound attachment travels as base64 inside a JSON callback (relay to nginx to Frappe), so every hop must accept about 4/3 of the file plus a little JSON. Raise them together:

| Hop | Setting | Default | Notes |
|---|---|---|---|
| Relay | `MEDIA_MAX_BYTES` | 15 MiB (15728640) | Raw file size. After base64 (x 4/3) and JSON the callback is about 20 MiB. Bigger media arrive as a text note. |
| nginx in the Frappe stack | `CLIENT_MAX_BODY_SIZE` (frappe_docker env var, `client_max_body_size` in its nginx template) | `50m` in the frappe_docker `compose.yaml` and docs I checked | Must be at least the ~20 MiB callback. Verify the name and default in your frappe_docker version. A plain nginx without this setting allows only 1m. |
| Frappe request limit | site config `max_file_size` (bytes) | 25 MiB (`frappe/app.py`) | Applies to the callback request. Must exceed the ~20 MiB callback. This is the **site config** value; System Settings does not change it for non-upload endpoints. |
| Frappe stored file | System Settings, **Max File Size (MB)** (`max_file_size`) | 25 MB | Checked when the attachment is saved as a File. If the file is over it, the message is kept with an "[attachment could not be saved]" note and an Error Log row. |

With the defaults the chain is consistent (15 MiB raw, about 20 MiB on the wire, 25 MiB Frappe, 50m nginx). To raise the cap, say to 30 MiB raw (about 40 MiB on the wire): set `MEDIA_MAX_BYTES=31457280` on the relay, `CLIENT_MAX_BODY_SIZE` to at least `60m`, site config `max_file_size` to at least 45 MiB (`bench --site <site> set-config max_file_size 47185920`), and System Settings Max File Size to at least 30 MB. Then restart the relay and the Frappe services. WhatsApp has its own media size limits, so there is little point raising it far.

If media from customers stops arriving, check in this order: the relay log for the callback status (413 means nginx or Frappe refused the body), then Error Log for "WhatsApp inbound media not saved".

## 4. WhatsApp Settings

The claim UI needs a MariaDB site (it serialises key changes with `GET_LOCK`). In Desk, as a System Manager, open **WhatsApp Settings**:

1. **Relay Base URL**: `http://whatsapp-relay:8800`.
2. **Callback URL**: Frappe as the relay sees it, e.g. `http://frontend:8080` (empty falls back to the site's own URL, which is rarely reachable from inside Docker). The relay sends inbound messages and status updates here, with the site name as `Host`.
3. **Relay Bootstrap Token**: the `RELAY_BOOTSTRAP_TOKEN` value, if you set one.
4. Save, then **Relay, Connect Relay**. Frappe generates a random key, stores it in `relay_api_key` and claims the relay. You never see or type the key. Before accepting the claim, the relay calls this site back at the Callback URL (with the new key and the site name as `Host`); if that fails, Connect Relay says the Callback URL is unreachable and nothing is claimed: fix the Callback URL, save, and try again.

Only a System Manager can change the Relay Base URL, API key, pending key or bootstrap token, and a typed API key is only accepted while the relay is in env mode.

The **Relay Status** panel (and **Relay, Test Connection**) shows the state:

| State | Meaning |
|---|---|
| Connected | Relay reachable and the key is accepted. Also shows mode, version, numbers connected / linked, callbacks pending / dead, callback URL and site name. |
| Unreachable | Connection error or timeout. Check the Relay Base URL and that the container is up. |
| Key rejected | The relay answered 401: its key is not the one stored here. See recovery below. |
| Unclaimed | The relay is waiting for **Connect Relay**. |

Day-two buttons (shown when the relay is claimed and connected):

- **Rotate Key**: generates a new key and swaps it in. The old key keeps working for other routes for 10 minutes, then is refused.
- **Update Callback URL**: after changing the Callback URL field, pushes it to the relay without re-claiming. The relay checks the new URL the same way as on claim and keeps the old one if it is unreachable. The new URL also applies to callbacks already queued (for example after moving the site, queued callbacks go to the new address).
- **Replay Dead Callbacks**: appears when the panel shows dead callbacks (ones the relay gave up on, e.g. while the Callback URL was wrong). Sends them to this site again, oldest first.
- **Retry Pending Key**: appears if a Connect or Rotate was sent but its answer was lost (the panel warns about it). Retrying finishes the same change; do it before anything else.

If the relay reports `already_claimed` on Connect, it already has a key. If this site holds that key, the panel shows Connected and there is nothing to connect (use Rotate Key for a new key). If not (another site or an earlier install claimed it), see recovery below. If the relay says its key comes from `RELAY_API_KEY` (env mode), type that value into **Relay API Key** by hand; the field is editable only for that case.

**Recovery from "Key rejected", env mode** (the panel shows mode `env`): the key is the relay's `RELAY_API_KEY` environment variable. Type that exact value into **Relay API Key** and save. Do not wipe anything.

**Recovery from "Key rejected", claimed mode** (key lost, relay claimed by another site, restored old backup): the claim is stored in the relay volume, so reset it. Stop the relay, wipe its volume, start it, and click **Connect Relay** again. Wiping the volume also deletes the device keys, so **every number must be relinked** (step 5). There is no other way to reclaim a relay without its key.

## Template parameters

CRM sends templates with their `{{1}}`, `{{2}}`... placeholders. Each template needs parameter rows so these are filled from the message's reference document (Lead, Deal, Customer, Sales Order, any doctype):

- On the **WhatsApp Templates** form, add one row per placeholder in **Parameters**: `Placeholder` (1, 2, ...), `Field`, optional `Default Value`.
- `Field` is a fieldname on the reference doctype, or `link_field.target_field` for one level of link, e.g. `organization` or `lead.first_name` on a Deal.
- Not allowed as sources: Password, Attach, Attach Image, Code, Signature, Geolocation, JSON, table and layout fields, `owner` and `modified_by` (staff email addresses, also not through a link), and User fields other than `full_name`, `first_name`, `last_name`.
- The sending user must be able to read the reference document, and any document a `link.field` reads through; otherwise the send is refused with a permission error.
- Saving the template fails if a `{{n}}` has no row, numbering has gaps, or a field does not exist on the template's doctype.
- A message with no reference document falls back to the CRM Contact, Lead or Deal matched by phone number.
- The `Default Value` is used when the field is empty. If there is still no value, no row, or no reference document, **the send is blocked** with an error naming the placeholders (for example "needs {{2}} (organization) but CRM Lead LEAD-0001 has no value"). Nothing ever goes out with a literal `{{1}}`.

## 5. Link numbers

Create a **WhatsApp Account** per number, then click **Connect / Generate QR** and scan from WhatsApp, Linked Devices, Link a device. Alternatively use the pairing-code option on the account form, enter the phone number, and type the code into the phone (Linked Devices, Link with phone number instead). Sessions are stored in the volume and restored on restart: no re-scan needed.

Default number: you do not choose one. WhatsApp Settings' **Default Outgoing / Incoming Account** exist only so CRM shows WhatsApp; CRM shows WhatsApp only while the default outgoing account is **Active**, so both are kept pointing at an Active number automatically: filled when the first number becomes Active, and moved to another Active number when theirs disconnects, is rate-limited, paused, logged out or deleted. If no number is Active, CRM hides WhatsApp until one reconnects. Sending rotates across all Active numbers, regardless of that field, and replies do not stick to one number.

Proxy: set **Proxy** on the account before connecting. Changing or clearing it later applies only on the next connect: click **Pause**, then **Resume**.

Migrating from the old Node relay: every account must be relinked once, because the Node relay's sessions cannot be reused. Unlink the old "Chrome" linked device on each phone first to free a slot (WhatsApp allows a limited number of linked devices).

## 6. Back up the data volume

The `whatsapp-relay-data` volume holds `relay.db`: device keys for every linked number, plus the pending callback queue. If it is lost, every number must be relinked. If it leaks, whoever has it can act as your numbers: encrypt backups and restrict access like credentials.

The volume name is prefixed with the compose project name; find it with `docker volume ls`.

```bash
docker compose stop whatsapp-relay   # consistent snapshot; SQLite runs in WAL mode
docker run --rm -v <project>_whatsapp-relay-data:/data -v "$PWD":/backup alpine \
  tar czf /backup/whatsapp-relay-data-$(date +%F).tgz -C /data .
docker compose start whatsapp-relay
```

Restoring a backup onto a second running relay while the first is alive makes WhatsApp replace one stream with the other. Stop the old one first.

## 7. Warm up new numbers

A fresh number that immediately sends bulk traffic is the most reliable way to get it restricted. Follow `docs/warm-up.md`: real SIM numbers only, start at `daily_limit` 20 and roughly double every 3-4 days over 1-2 weeks, mix in inbound conversations early, avoid identical bulk text, keep the send delay, and know what a TemporaryBan alert means. Risk reduction only.

## 8. Risk reduction, not ban-proof

This setup uses an unofficial client. WhatsApp can restrict, temporarily ban or permanently ban any number at any time, and nothing here prevents that. The warm-up, delays, caps, failover and per-account proxy support reduce risk; they do not remove it. Do not describe this system as ban-proof or Meta-approved, and do not link a number you cannot afford to lose.

## 9. Hosts that cannot run extra containers

Managed Frappe hosting (e.g. Frappe Cloud) does not let you add a sidecar container. Run the relay on a separate host (a small VM with Docker works) and point `relay_base_url` at it. Then:

- Put it behind TLS and restrict access (firewall to your Frappe host's IP, or a private network/VPN). The API key is the only other defence.
- Set the **Callback URL** in WhatsApp Settings to your site's public HTTPS URL, reachable from the relay host, before **Connect Relay**. The site name sent as `Host` is then usually irrelevant. Set `RELAY_BOOTSTRAP_TOKEN`, since the port is reachable from outside the compose network.
- Publish the port only on the private interface or behind the reverse proxy; the same backup and one-relay-per-volume rules apply.

## Releasing a new version

Each release is one git tag that drives both images, so the app and the relay never drift apart.

1. Merge to the release branch, then tag: `git tag v1.2.0 && git push origin v1.2.0`.
2. GitHub Actions builds and publishes `ghcr.io/onebook-erp/yourwhatsapp-relay:v1.2.0` (amd64 + arm64). Check the **Relay image** workflow run is green. **First release only:** in GitHub, open the org's Packages → `yourwhatsapp-relay` → Package settings → Change visibility → **Public** (new packages start private, and servers pull without logging in).
3. On the server:
   - set yourwhatsapp to `v1.2.0` in `apps.json` and rebuild the Frappe image as usual;
   - set `RELAY_VERSION=v1.2.0` in the compose `.env`;
   - `docker compose ... pull && docker compose ... up -d` (same file list as step 3);
   - `docker compose exec backend bench --site <site> migrate`.
4. Check WhatsApp Settings → **Test Connection** shows Connected and the new relay version.

Only containers whose image changed are recreated. A release that changes only the app restarts the Frappe containers and leaves the relay (and every linked number) connected; the relay's callback queue covers the moment Frappe is down. A release that changes the relay restarts it gracefully and it restores every number without a rescan.

**Rollback:** set the previous tag in `apps.json` and `RELAY_VERSION`, rebuild/pull, `up -d`. Back up the relay volume (section 6) before upgrading the relay, since a newer relay may add tables to `relay.db`.
