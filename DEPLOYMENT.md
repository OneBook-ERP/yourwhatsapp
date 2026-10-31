# Deploying yourwhatsapp (Docker)

Production runs two things: your Frappe stack (built from [frappe_docker](https://github.com/frappe/frappe_docker)) with the `yourwhatsapp` app installed, and the Go relay as its own container next to it. This is an internal ops tool, not approved by Meta/WhatsApp. The Docker files were written without Docker available on the dev host: treat the first deploy as the first test.

## 1. Build the Frappe image with yourwhatsapp

Add the app to the `apps.json` you feed to frappe_docker's image build:

```json
[
  {"url": "https://github.com/frappe/crm", "branch": "version-16"},
  {"url": "https://github.com/OneBook-ERP/yourwhatsapp", "branch": "version-16"}
]
```

```bash
export APPS_JSON_BASE64=$(base64 -w 0 apps.json)
docker build --build-arg=APPS_JSON_BASE64=$APPS_JSON_BASE64 \
  --build-arg=FRAPPE_BRANCH=version-16 \
  --tag=<registry>/frappe-crm:<version> --file=images/layered/Containerfile .
```

Then `bench --site <site> install-app yourwhatsapp` (frappe_docker: `docker compose exec backend bench --site <site> install-app yourwhatsapp`). If your host cannot add containers (e.g. Frappe Cloud), see section 9.

## 2. Build and push the relay image

```bash
cd apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay
docker build -t ghcr.io/<org>/yourwhatsapp-relay:<version> .
docker push ghcr.io/<org>/yourwhatsapp-relay:<version>
```

The image is a static Go binary on `gcr.io/distroless/static:nonroot`, listening on 8800, with its data in `/data`. Pin a version tag rather than `latest`.

## 3. Add the compose override

Copy `yourwhatsapp/yourwhatsapp/relay/compose.whatsapp.yaml` next to your frappe_docker compose files, set the image name (or uncomment `build:`), and add it to the stack:

```bash
export SITE_NAME=crm.example.com
export RELAY_API_KEY=$(openssl rand -hex 32)   # keep it, you need it in step 4
docker compose -f compose.yaml -f overrides/compose.mariadb.yaml -f overrides/compose.redis.yaml \
  -f compose.whatsapp.yaml up -d
```

- `FRAPPE_BASE_URL` is the address of Frappe's internal web service as seen from the relay (frappe_docker: `http://frontend:8080`). `FRAPPE_SITE_NAME` is sent as the `Host` header so the callback hits the right site.
- No ports are published. Only containers on the compose network (your Frappe services) reach the relay. Never expose port 8800 to the internet: with the key, anyone can send messages as your numbers.
- `stop_grace_period: 30s` lets the relay close WhatsApp sessions cleanly. Do not shorten it, and avoid `docker kill` on a live relay: repeated abrupt disconnects can get a device unlinked.
- Run exactly one relay per data volume.
- `MEDIA_MAX_BYTES` (default 15 MiB) caps inbound attachments; bigger ones arrive as a text note. Keep it at or below the site's max file size (System Settings), or Frappe stores the message with an "[attachment could not be saved]" note instead.
- The relay runs as uid 65532 (distroless nonroot). A fresh named volume inherits the right ownership from the image; a bind mount or a pre-existing volume needs a host-side `chown -R 65532:65532 <path>` or the relay cannot open `relay.db` and crashes on start.

## 4. WhatsApp Settings

In Frappe desk open **WhatsApp Settings** and set:

- `relay_base_url`: `http://whatsapp-relay:8800`
- `relay_api_key`: the same value as `RELAY_API_KEY`

## 5. Link numbers

Create a **WhatsApp Account** per number, then click **Connect / Generate QR** and scan from WhatsApp, Linked Devices, Link a device. Alternatively use the pairing-code option on the account form, enter the phone number, and type the code into the phone (Linked Devices, Link with phone number instead). Sessions are stored in the volume and restored on restart: no re-scan needed.

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

A fresh number that immediately sends bulk traffic is the most reliable way to get it restricted. Start with a few real conversations a day with people who expect to hear from you, raise volume gradually over several weeks, keep the daily cap per account low, spread load across numbers, and prefer replies to cold outreach. The relay adds randomized delays between sends (`SEND_DELAY_MIN` / `SEND_DELAY_MAX`); do not set them to zero.

## 8. Risk reduction, not ban-proof

This setup uses an unofficial client. WhatsApp can restrict, temporarily ban or permanently ban any number at any time, and nothing here prevents that. The warm-up, delays, caps, failover and per-account proxy support reduce risk; they do not remove it. Do not describe this system as ban-proof or Meta-approved, and do not link a number you cannot afford to lose.

## 9. Hosts that cannot run extra containers

Managed Frappe hosting (e.g. Frappe Cloud) does not let you add a sidecar container. Run the relay on a separate host (a small VM with Docker works) and point `relay_base_url` at it. Then:

- Put it behind TLS and restrict access (firewall to your Frappe host's IP, or a private network/VPN). The API key is the only other defence.
- Set `FRAPPE_BASE_URL` to your site's public HTTPS URL, reachable from the relay host; `FRAPPE_SITE_NAME` is usually unnecessary then.
- Publish the port only on the private interface or behind the reverse proxy; the same backup and one-relay-per-volume rules apply.
