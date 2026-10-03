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

Configuration is environment-only; see `config.go` for every variable.

```bash
gofmt -l . && go vet ./... && go test -race ./...
docker build -t gcalendar-recording-bot .
```
