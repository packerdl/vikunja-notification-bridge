# vikunja-notification-bridge

A small service that turns [Vikunja](https://vikunja.io) task reminders into
[ntfy](https://ntfy.sh) notifications. Acknowledging the notification
completes the task in the Vikunja API.

```
task.reminder.fired ──► bridge ──► ntfy notification
                                        │
                                        ▼ acknowledge
                    bridge ◄── POST /v1/done/<secret>?task=<id>
                        │
                        └──► Vikunja API: GET task, POST it back whole with done flipped
```

## Endpoints

| Endpoint | Caller | Auth | Behavior |
|---|---|---|---|
| `GET /healthz` | reverse proxy / ops | none | `200 ok` |
| `POST /v1/reminder/<secret>` | Vikunja webhook | path secret (+ optional HMAC) | `204` fast, publish happens in the background, duplicates deduped within ~2 min |
| `POST /v1/done/<secret>?task=<id>` | callback | path secret | `200` when the task is completed |

## Configuration

All settings live in the environment, see `.env.example` for the full list.

## Secrets

```
REMINDER_SECRET=$(openssl rand -hex 32)
DONE_SECRET=$(openssl rand -hex 32)
```

The two secrets must differ. The code rejects equal values, so a leak of the
callback path cannot forge reminder events.

Vikunja token scopes: **Tasks → Read one, Tasks → Update**. Both are needed.
A POST update wipes every field the body does not include, so the done
endpoint reads the task first and posts the whole record back with done
flipped. ntfy token scope: publish for the bridge topic.

## Webhook registration

Vikunja → Settings → Webhooks:

```
Target:  http://bridge:8080/v1/reminder/<REMINDER_SECRET>
Events:  task.reminder.fired
```

The bridge and Vikunja must share a Docker network, or use the public URL.
Optionally set a secret on the webhook and the same value in
`VIKUNJA_WEBHOOK_SECRET` to enable `X-Vikunja-Signature` verification.

## Public route

The callback needs a public HTTPS route:

```
notifications.example.com {
    reverse_proxy <bridge-host-ip>:18080
}
```

`PUBLIC_BASE_URL` must match the hostname.

## Image builds

GitHub Actions builds `linux/amd64` and `linux/arm64` images and pushes them
to GHCR (`ghcr.io/packerdl/vikunja-notification-bridge`) on `main` and on
`v*` tags. No extra configuration needed, the workflow token handles it. Pulls
outside GitHub require a token with `read:packages` unless the image is
public.

## Local testing

```sh
# notification, simulates the Vikunja webhook payload
# renders on the phone as: Reminder at Tue, 10/27 at 07:00 · *Inbox*
curl -s http://127.0.0.1:8080/v1/reminder/<SECRET> -H 'Content-Type: application/json' \
  -d '{"event_name":"task.reminder.fired","time":"2026-10-27T11:00:00Z","data":{\
"project":{"title":"Inbox"},"task":{"id":42,"title":"Test"},"reminder":{"reminder":"2026-10-27T07:00:00-04:00"}}}'

# callback, simulates a notification button
curl -s -X POST "http://127.0.0.1:8080/v1/done/<SECRET>?task=42"
```
