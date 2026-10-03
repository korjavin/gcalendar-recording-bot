# gcalendar-recording-bot: design

The system-wide contract (recorders, events, transcriber hand-off) is in
[`architecture.md`](architecture.md). This file covers what is specific to the
calendar bot.

## 1. What a user does

1. Opens the bot's web page and clicks **Connect Google Calendar**. Google asks
   for read-only access to their calendar. Done.
2. Invites the bot's address (`BOT_INVITE_EMAIL`, e.g. `notetaker@example.com`)
   to any meeting they want recorded, like inviting a colleague.
3. Gets e-mails: "admit NoteTaker from the lobby" when the bot knocks, and
   "Transcript ready: <link>" when the transcript is in Outline (or a one-line
   failure note).

The bot has no Google account. `BOT_INVITE_EMAIL` is only a marker the bot looks
for among a meeting's attendees; it joins calls as an anonymous guest. The
address must be a deliverable mailbox (an alias or a plus-address is enough),
otherwise every invitation bounces back to the organizer.

To stop, the user removes the bot's access in their Google account
(Security → third-party access). The next poll sees `invalid_grant`, drops the
connection and e-mails them that it is disconnected.

## 2. Which meetings are recorded

An event in a connected calendar is recorded when all hold:

* it is not cancelled;
* `BOT_INVITE_EMAIL` is among its attendees (case-insensitive);
* it has a call link: Google Meet from `conferenceData` (video entry point) or
  `hangoutLink`, or a Jitsi link under `JITSI_BASE_URL` found in `location` or
  `description`. A Jitsi link wins when both are present: Google Calendar adds a Meet
  conference to new events on its own, so an explicit Jitsi link is the intent.

One recording per meeting occurrence, even when several attendees connected
their calendars: the job id is `cal-` + the first 16 hex chars of
`sha256(iCalUID + "|" + start time in RFC 3339 UTC)`. `iCalUID` is the same in
every attendee's copy of the event; `start` separates the occurrences of a
recurring meeting. Everyone who connected a calendar containing that occurrence
is on the job's notification list.

## 3. Web page and Google OAuth

* `GET /` — one page: what the bot does, the invite address, the **Connect**
  button. It shows nothing about anybody's meetings, so it needs no login.
* `GET /connect` — redirect to Google's consent screen: authorization-code flow,
  `access_type=offline`, `prompt=consent`, scopes `openid email
  https://www.googleapis.com/auth/calendar.readonly`, a random `state` also set
  in a short-lived `HttpOnly; Secure; SameSite=Lax` cookie.
* `GET /oauth/callback` — check `state` against the cookie, exchange the code,
  read the e-mail from the ID token (or the userinfo endpoint), check it against
  `ALLOWED_EMAIL_DOMAINS`, store the connection, show "Connected as …".
  Connecting again replaces the stored token.

**Who may connect.** Every connected calendar can send recordings into the one
Outline knowledge base, so connecting is limited to `ALLOWED_EMAIL_DOMAINS`
(required, comma-separated; the service refuses to start without it).
Recommended on top: make the Google OAuth consent screen **Internal** to the
Workspace, so Google itself only lets that organisation's accounts consent; an
Internal app also needs no verification and its refresh tokens do not expire
(an External app in *Testing* status loses them after 7 days).

**Tokens at rest.** Refresh tokens are encrypted with AES-256-GCM under
`TOKEN_KEY` (32 bytes, base64; required) and stored in
`DATA_DIR/connections/<sha256(email)>.json`, mode 0600. Never logged.

## 4. Polling and scheduling

* Every `POLL_INTERVAL_S` (300): for each connection, refresh the access token,
  `events.list` on `primary` with `singleEvents=true`, `orderBy=startTime`,
  `timeMin=now`, `timeMax=now+24h`. Collect candidates (§2) across all
  connections, merged by job id.
* A candidate becomes a scheduled job (`DATA_DIR/jobs/<id>/job.json`, state
  `scheduled`) with title (event summary), url, start, end, notify list.
* Each poll reconciles: a scheduled (not yet started) job whose occurrence is
  gone, cancelled, moved (new id) or no longer invites the bot is dropped.
* A scheduler loop starts each job `JOIN_LEAD_S` (90) before its start:
  `POST /recordings` with `join_timeout_s` = `JOIN_TIMEOUT_S` (1200),
  `max_duration_s` = event length + `OVERRUN_S` (1800), `empty_grace_s` =
  `EMPTY_GRACE_S` (60). From then on the job follows `architecture.md` §3–§5,
  as in any orchestrator.
* An accepted `recording.finished` stores the transcriber body in `job.json`
  together with the `finished` state, before anything is sent; `handed_off`
  is set once the transcriber answers `2xx`. Delivery retries on `5 s, 15 s,
  45 s, 2 min, 5 min`, then an hourly sweep and every startup send whatever is
  still not handed off. Redirects are not followed.
* A watchdog runs every minute. A job's deadline is the time it was started
  + `join_timeout_s` + `max_duration_s` + 10 min; a job still `starting` or
  `started` after it is checked with a signed `GET /recordings/{id}`: still
  running → checked again 10 min later; finished/failed → the job record is
  taken as the lost event; `404` or unreachable three checks in a row → failed
  `lost`, with the failure e-mail. After a restart, running jobs stay under
  the watchdog; scheduled ones are re-planned by the next poll.
* `invalid_grant` on refresh → delete the connection, e-mail its owner once.

## 5. Notifications (e-mail)

Plain-text mail through `SMTP_HOST`/`SMTP_PORT`/`SMTP_USER`/`SMTP_PASSWORD`
(STARTTLS), from `SMTP_FROM`, to every address on the job's notify list.
`SMTP_HOST` is optional: empty disables e-mail.

| trigger | subject |
|---|---|
| `recording.waiting_admission` | NoteTaker is waiting in the lobby: <title> |
| `recording.finished` shorter than `MIN_RECORDING_S` | Recording too short: <title> |
| `recording.failed` / watchdog `lost` | Recording failed: <title> (one-line reason) |
| `POST /notify` from tr2outline | Transcript ready: <title> (the `content` as body) |
| connection dropped (`invalid_grant`) | Calendar disconnected |

Sending is best effort with a few retries; a lost e-mail never blocks a job.

## 6. Endpoints

| endpoint | caller | auth |
|---|---|---|
| `GET /`, `GET /connect`, `GET /oauth/callback` | people (public via Traefik) | none / OAuth `state` |
| `POST /events` | recorders, at `PUBLIC_URL` (via Traefik) | `x-recorder-signature` |
| `POST /notify` | tr2outline, at `PUBLIC_URL` | `x-jitsi-capture-signature` (`WEBHOOK_SECRET`) |
| `GET /health` | anyone | none |

## 7. Not now

* Several calendars per person (only `primary`).
* Push notifications from Google (`events.watch`) instead of polling.
* A page listing a person's upcoming recordings (would need a login).
