# gcalendar-recording-bot

Records the meetings you invite it to. Connect your Google Calendar on the
bot's web page (read-only OAuth), then add the bot's address
(e.g. `notetaker@example.com`) as an attendee to any Google Meet or Jitsi
meeting. The bot joins the call, records it through the matching recorder
service, hands the audio to the transcriber, and e-mails you when the
transcript is ready.

- [`docs/design.md`](docs/design.md) — this service: connect flow, which
  meetings are recorded, polling and scheduling, e-mail, endpoints.
- [`docs/architecture.md`](docs/architecture.md) — the contract shared with the
  recorders and the transcriber.

## Build and test

```bash
gofmt -l . && go vet ./... && go test -race ./...
docker build -t gcalendar-recording-bot .
```

## Configuration

Environment only; [`.env.example`](.env.example) lists every variable with a
placeholder and a comment, and `config.go` is the single reader. Required:
`PUBLIC_URL`, `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `TOKEN_KEY`,
`ALLOWED_EMAIL_DOMAINS`, `BOT_INVITE_EMAIL`, `RECORDER_SECRET`, `WEBHOOK_URL`,
`WEBHOOK_SECRET`, and at least one of `JITSI_RECORDER_URL` / `MEET_RECORDER_URL`.

Generate the secrets:

```bash
openssl rand -base64 32   # TOKEN_KEY (exactly 32 bytes, base64)
openssl rand -hex 32      # RECORDER_SECRET, WEBHOOK_SECRET (must match the peers)
```

`TOKEN_KEY` encrypts the stored refresh tokens; changing it makes every
connection unreadable and users have to connect again.

**E-mail is optional.** Leave `SMTP_HOST` empty and the bot sends no mail at all
(recordings and transcripts still happen, but nobody is told). With
`SMTP_HOST` set, `SMTP_FROM` is required; the bot speaks plain SMTP on
`SMTP_PORT` (default 587), upgrades with STARTTLS when the server offers it,
and authenticates with `SMTP_USER`/`SMTP_PASSWORD` when `SMTP_USER` is set.
Implicit TLS (port 465) is not supported.

## Google Cloud setup

1. **Project.** In the Google Cloud console create a project (or pick one) in
   the Workspace organisation whose people will use the bot.
2. **Enable the API.** *APIs & Services → Library → Google Calendar API →
   Enable.*
3. **OAuth consent screen.** *APIs & Services → OAuth consent screen* (Google
   Auth Platform → Branding / Audience).
   - User type **Internal** — recommended. Only accounts of your Workspace can
     consent, the app needs no Google verification, and refresh tokens do not
     expire. An **External** app left in *Testing* status loses every refresh
     token after 7 days, so users would silently be disconnected each week;
     External in *Production* needs Google's verification for the calendar
     scope.
   - App name (e.g. "NoteTaker"), support e-mail, and your domain
     (e.g. `example.com`) as an authorised domain.
4. **Scopes.** Add `openid`, `.../auth/userinfo.email` and
   `https://www.googleapis.com/auth/calendar.readonly`. The bot never asks for
   write access.
5. **OAuth client.** *APIs & Services → Credentials → Create credentials →
   OAuth client ID*, type **Web application**. Authorised redirect URI:
   `PUBLIC_URL/oauth/callback`, e.g.
   `https://notetaker.example.com/oauth/callback` — it must match `PUBLIC_URL`
   exactly (scheme, host, no trailing slash). Put the client ID and secret into
   `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET`.
6. **Invite address.** `BOT_INVITE_EMAIL` is only a marker the bot looks for
   among attendees; it has no Google account. It must still be a deliverable
   mailbox — an alias or a plus-address (`you+notetaker@example.com`) is
   enough — otherwise every invitation bounces back to the organiser.
7. **Allowed domains.** Set `ALLOWED_EMAIL_DOMAINS` (e.g. `example.com`): only
   these accounts may connect, even with an Internal consent screen.

## Deploy

`docker-compose.yml` runs the bot behind Traefik on the shared external network
(`TRAEFIK_NETWORK_NAME`, default `traefik`), which the recorders and the
transcriber are on too. Traefik routes `${DOMAIN}` paths `/`, `/connect`,
`/oauth/callback`, `/events`, `/notify` and `/health` to the bot; `/events` and
`/notify` are HMAC-signed callbacks from the recorders and the transcriber,
which reach the bot at `PUBLIC_URL`. State (encrypted connections, jobs) lives
in the named volume `gcalendar-recording-bot-data` at `/data`.

Locally:

```bash
cp .env.example .env    # fill in real values
docker compose up -d
```

Continuous deployment: every push to `master` runs
`.github/workflows/deploy.yml`, which builds and pushes
`ghcr.io/<owner>/gcalendar-recording-bot:<sha>` (and `:latest`), rewrites the
image line on the `deploy` branch to that sha, and calls the Portainer webhook.
In Portainer, add a git stack from this repository on branch `deploy`, compose
file `docker-compose.yml`, with the variables from `.env.example` in the stack
environment, and enable its redeploy webhook. Store the webhook URL as the
repository secret `PORTAINER_REDEPLOY_HOOK`; without it the workflow still
updates `deploy` and only skips the webhook call.

## Using it

- **Connect:** open `PUBLIC_URL`, click **Connect Google Calendar**, sign in
  with an account from `ALLOWED_EMAIL_DOMAINS` and allow read-only calendar
  access. The page answers "Connected as …". Connecting again replaces the
  stored token.
- **Record a meeting:** add `BOT_INVITE_EMAIL` as an attendee of a meeting with
  a Google Meet link (or a Jitsi link under `JITSI_BASE_URL` in the location or
  description). The calendar is read every `POLL_INTERVAL_S`, and the recorder
  joins `JOIN_LEAD_S` before the start.
- **Disconnect:** remove the app in your Google account (*Security → Your
  connections to third-party apps & services*). On its next poll the bot drops
  the connection and e-mails you that the calendar is disconnected.

## Smoke test

1. `curl https://notetaker.example.com/health` answers `200`.
2. Open the page, connect your calendar, and see "Connected as …".
3. Create a Google Meet meeting starting in about 5 minutes and invite
   `BOT_INVITE_EMAIL`. Wait for the next poll (up to `POLL_INTERVAL_S`), or
   restart the container to poll at once.
4. Join the meeting yourself. When NoteTaker knocks you get a "waiting in the
   lobby" e-mail — admit it, talk for a minute or more (`MIN_RECORDING_S`),
   then leave.
5. After the call you get "Transcript ready: <meeting title>", and the Outline
   document carries the meeting title.
