package main

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// webhook is the transcriber's input (architecture.md §5). The Zulip-only
// fields are never sent.
type webhook struct {
	Event            string   `json:"event"`
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	JitsiURL         string   `json:"jitsi_url"` // the call link, any platform
	Source           string   `json:"source,omitempty"`
	AudioPath        string   `json:"audio_path"`
	DurationS        float64  `json:"duration_s"`
	StartedAt        string   `json:"started_at"`
	EndedAt          string   `json:"ended_at"`
	Participants     []string `json:"participants"`
	CallbackURL      string   `json:"callback_url"`
	Tracks           []track  `json:"tracks,omitempty"`
	SpeakerHintsPath string   `json:"speaker_hints_path,omitempty"`
}

type track struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Path    string  `json:"path"`
	OffsetS float64 `json:"offset_s"`
	EndedS  float64 `json:"ended_s"`
}

// Pauses before each retry of the transcriber POST; tests shorten them.
var webhookRetries = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 2 * time.Minute, 5 * time.Minute}

// handOffs holds the job ids with a delivery in flight, so a sweep does not
// start a second one beside the retries.
var handOffs sync.Map

func buildWebhook(cfg *Config, j *job, ev *recEvent) *webhook {
	w := &webhook{
		Event:        "recording.finished",
		ID:           j.ID,
		Title:        j.Title,
		JitsiURL:     j.URL,
		DurationS:    ev.DurationS,
		StartedAt:    ev.StartedAt,
		EndedAt:      ev.EndedAt,
		Participants: ev.Participants,
		CallbackURL:  cfg.PublicURL + "/notify",
	}
	if w.Participants == nil {
		w.Participants = []string{}
	}
	if meetURL(j.URL) != "" {
		w.Source = "meet"
	}
	for _, a := range ev.Artifacts { // unknown kinds are ignored
		switch a.Kind {
		case "audio":
			w.AudioPath = a.Path
		case "track":
			w.Tracks = append(w.Tracks, track{a.ParticipantID, a.Name, a.Path, a.OffsetS, a.EndedS})
		case "captions":
			w.SpeakerHintsPath = a.Path
		}
	}
	return w
}

// handOff sends a finished job's stored webhook to the transcriber, retrying
// on webhookRetries, and marks the job handed off on a 2xx. What is still
// undelivered after the last retry waits for the next sweep.
func handOff(cfg *Config, id string) {
	if _, busy := handOffs.LoadOrStore(id, true); busy {
		return
	}
	defer handOffs.Delete(id)

	jobsMu.Lock()
	j, err := readJob(cfg.DataDir, id)
	jobsMu.Unlock()
	if err != nil || j.Webhook == nil || j.HandedOff {
		return
	}
	body, err := json.Marshal(j.Webhook)
	if err != nil {
		return
	}
	hdr := map[string]string{
		"x-jitsi-capture-event":     "recording.finished",
		"x-jitsi-capture-signature": sign(cfg.WebhookSecret, body),
	}
	// Every failure is retried, a 4xx too: the transcriber or its secret
	// may be fixed by a redeploy, and it is idempotent on id.
	for attempt := 0; ; attempt++ {
		err = postOnce(context.Background(), cfg.WebhookURL, hdr, body)
		if err == nil {
			break
		}
		if attempt >= len(webhookRetries) {
			slog.Warn("transcriber hand-off failed; the hourly sweep retries", "job", id, "err", err)
			return
		}
		slog.Info("transcriber hand-off retry", "job", id, "attempt", attempt+1, "err", err)
		time.Sleep(webhookRetries[attempt])
	}

	jobsMu.Lock()
	defer jobsMu.Unlock()
	if j, err = readJob(cfg.DataDir, id); err != nil {
		return
	}
	j.HandedOff = true
	if err := writeJob(cfg.DataDir, j); err != nil {
		slog.Error("write job failed", "job", id, "err", err) // resent later; harmless
		return
	}
	slog.Info("handed off to transcriber", "job", id)
}

// runHandOffs re-drives undelivered hand-offs now (a restart) and hourly.
func runHandOffs(ctx context.Context, cfg *Config) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		for _, id := range pendingHandOffs(cfg) {
			go handOff(cfg, id)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func pendingHandOffs(cfg *Config) []string {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	ents, _ := os.ReadDir(filepath.Join(cfg.DataDir, "jobs"))
	var ids []string
	for _, e := range ents {
		if j, err := readJob(cfg.DataDir, e.Name()); err == nil && j.Webhook != nil && !j.HandedOff {
			ids = append(ids, j.ID)
		}
	}
	return ids
}

// handleNotify is POST /notify from tr2outline: the transcript is published.
func handleNotify(w http.ResponseWriter, r *http.Request, cfg *Config) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if !hmac.Equal([]byte(r.Header.Get("x-jitsi-capture-signature")), []byte(sign(cfg.WebhookSecret, body))) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var n struct{ ID, Content string }
	if err := json.Unmarshal(body, &n); err != nil || n.ID == "" || n.Content == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}
	var j *job
	if jobIDRe.MatchString(n.ID) {
		jobsMu.Lock()
		j, _ = readJob(cfg.DataDir, n.ID)
		jobsMu.Unlock()
	}
	if j == nil || j.Webhook == nil {
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	slog.Info("transcript ready", "job", j.ID)
	notifier.mailTranscript(j.ID, j.Notify, j.Title, n.Content)
	w.WriteHeader(http.StatusOK)
}
