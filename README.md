# vikunja-notification-bridge

A small service that receives [Vikunja](https://vikunja.io) task reminder
webhooks and publishes notifications to self-hosted [ntfy](https://ntfy.sh), with a callback endpoint that completes the task in the Vikunja
API when the notification is acknowledged.

```
task.reminder.fired ──► bridge ──► ntfy notification
                                        │
                                        ▼ acknowledge
                    bridge ◄── POST /v1/done/<secret>?task=<id>
                        │
                        └──► Vikunja API: POST /api/v1/tasks/{id} {"done":true}
```

## Notification target

ntfy is the consumer today. Publishing is isolated in a single function, so a
different consumer can replace it behind the same reminder webhook without
changing the endpoints.

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

The two secrets must differ, the code refuses equal values, so leaking the
callback path cannot forge reminder events.

Vikunja token scope: **Tasks → Update** only. ntfy token scope: publish for
the bridge topic.

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
to Docker Hub on `main` and on `v*` tags. Configure the repository variable
`DOCKERHUB_NAMESPACE` and the secret `DOCKERHUB_TOKEN`.

## Validation notes

- Callback calls `POST /api/v1/tasks/{id}` with body `{"done":true}`
  (PUT and PATCH return 405).
- Reminder body renders as `Reminder at Tue, 10/27 at 07:00 · *Inbox*`.

## Local testing

```sh
# notification, simulates the Vikunja webhook payload
curl -s http://127.0.0.1:8080/v1/reminder/<SECRET> -H 'Content-Type: application/json' \
  -d '{"event_name":"task.reminder.fired","time":"2026-10-27T11:00:00Z","data":{\
"project":{"title":"Inbox"},"task":{"id":42,"title":"Test"},"reminder":{"reminder":"2026-10-27T07:00:00-04:00"}}}'

# callback, simulates a notification button
curl -s -X POST "http://127.0.0.1:8080/v1/done/<SECRET>?task=42"
```
