# Warm-up policy for new WhatsApp numbers

Operational policy, not code: nothing here changes a default. This reduces risk of a restriction or ban. It does **not** prevent one, and the system is an unofficial client that WhatsApp can restrict at any time. Do not link a number you cannot afford to lose.

## Before linking

- Use **real SIM-registered numbers**, not VOIP or virtual numbers.
- Let the number live a normal life for a few days first: profile name and photo, a few conversations on the phone.
- Link it on the phone's own network if you can; never run two relays against the same number.

## Daily cap ramp

A new `WhatsApp Account` defaults to `daily_limit` 200, which is too high for a fresh number. **Set it to 20 right after linking**, then raise it by hand:

| Day | `daily_limit` |
|---|---|
| 1-3 | 20 |
| 4-7 | 40 |
| 8-10 | 80 |
| 11-14 | 150 |
| after 2 weeks | the target (not above 200 without a reason) |

Roughly double every 3-4 days, and only if the number stayed healthy. If it hits a warning or a ban, drop back and ramp slower. Routing skips an account once its daily count reaches `daily_limit`, so a low cap also pushes load to your other numbers: warm numbers in turn rather than all at once.

## Behaviour that matters as much as volume

- **Mix in inbound conversations early.** A number that only sends and never receives replies looks like a broadcaster. Prefer replies to people who wrote first, and messages to customers who expect to hear from you.
- **Avoid identical bulk text.** Do not send the same message to many contacts back to back. Vary wording and use template placeholders that carry real per-person data.
- **Keep the send delay.** The relay adds a randomized delay and a typing indicator between sends (`SEND_DELAY_MIN` / `SEND_DELAY_MAX`). Never set them to zero.
- Do not message numbers that are not on WhatsApp, or that have blocked you; repeated failures are a signal.

## What a TemporaryBan alert means

When WhatsApp puts a temporary ban on a number, the relay reports it and the account moves to **RateLimited** with an expiry (`rate_limited_until`). Every System Manager and WhatsApp Admin gets a bell notification and an email ("down", with the reason and expiry).

- Sending from that number stops; routing and failover use your other numbers.
- **Do nothing to the number.** Do not reconnect, re-pair or restart repeatedly: that makes it worse.
- When the ban expires the account **returns to Active automatically** (a scheduled job lifts it), and a "back online" bell and email follow.
- After recovery, treat the number as new: put its `daily_limit` back to 20 and ramp again.
- Repeated bans mean volume, content or the number itself is the problem. Slow down; stop using it if it keeps happening.
- A ban with no expiry that an admin set by hand is not lifted automatically.

Alerts are sent at most once per account per 15 minutes, and a manual Pause or Logout does not alert. Deployment notes: `DEPLOYMENT.md`.
