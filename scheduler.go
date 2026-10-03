package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

// Job states after "scheduled". Only the scheduler and the event handler
// write them, always under jobsMu.
const (
	stateStarting = "starting" // POST /recordings in flight (or interrupted by a restart)
	stateStarted  = "started"  // the recorder accepted the job
	stateFinished = "finished"
	stateFailed   = "failed"
)

// Pauses before each retry of POST /recordings; tests shorten them.
var recorderRetries = []time.Duration{5 * time.Second, 15 * time.Second}

// errPermanent marks a recorder answer that retrying cannot change.
var errPermanent = errors.New("rejected")

var errBadJob = errors.New("unreadable job file")

var jobIDRe = regexp.MustCompile(`^cal-[0-9a-f]{16}$`)

// handOff passes a finished recording to the transcriber.
// ponytail: log-only stub until the transcriber hand-off is built.
var handOff = func(cfg *Config, j *job, event []byte) {
	slog.Info("recording finished; hand-off pending", "job", j.ID)
}

func readJob(dataDir, id string) (*job, error) {
	data, err := os.ReadFile(jobPath(dataDir, id))
	if err != nil {
		return nil, err
	}
	var j job
	if err := json.Unmarshal(data, &j); err != nil || j.ID != id {
		return nil, errBadJob
	}
	return &j, nil
}

func writeJob(dataDir string, j *job) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(jobPath(dataDir, j.ID), data)
}

// sign is the x-recorder-signature value for body.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// runScheduler starts due jobs until ctx ends.
func runScheduler(ctx context.Context, cfg *Config) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		startDue(ctx, cfg, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// startDue starts every job whose join time (start - JoinLead) has come and
// whose end has not passed. A job left "starting" by a restart is started
// again; the recorder answers a repeated id with 200.
// ponytail: starts run one after another; a goroutine per start if a burst
// of simultaneous meetings ever delays joins.
func startDue(ctx context.Context, cfg *Config, now time.Time) {
	for _, j := range claimDue(cfg, now) {
		err := postRecording(ctx, cfg, j)
		finishStart(cfg, j, err)
	}
}

// claimDue moves due jobs to "starting" under jobsMu, so the planner can no
// longer rewrite or drop them.
func claimDue(cfg *Config, now time.Time) []*job {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	ents, err := os.ReadDir(filepath.Join(cfg.DataDir, "jobs"))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("list jobs failed", "err", err)
		}
		return nil
	}
	var due []*job
	for _, e := range ents {
		j, err := readJob(cfg.DataDir, e.Name())
		if err != nil || (j.State != stateScheduled && j.State != stateStarting) {
			continue
		}
		if now.Before(j.Start.Add(-cfg.JoinLead)) || !now.Before(j.End) {
			continue
		}
		j.State = stateStarting
		if err := writeJob(cfg.DataDir, j); err != nil {
			slog.Error("claim job failed", "job", j.ID, "err", err)
			continue
		}
		due = append(due, j)
	}
	return due
}

// finishStart records the outcome of POST /recordings, unless an event has
// already moved the job on.
func finishStart(cfg *Config, started *job, err error) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	j, rerr := readJob(cfg.DataDir, started.ID)
	if rerr != nil || j.State != stateStarting {
		return
	}
	if err != nil {
		slog.Warn("recording start failed", "job", j.ID, "err", err)
		j.State, j.Error = stateFailed, "recorder_unavailable"
	} else {
		slog.Info("recording started", "job", j.ID)
		j.State = stateStarted
	}
	if werr := writeJob(cfg.DataDir, j); werr != nil {
		slog.Error("write job failed", "job", j.ID, "err", werr)
	}
	if err != nil {
		notifier.mailFailed(j.ID, j.Notify, j.Title, "the recorder could not be reached")
	}
}

// recorderFor picks the recorder by the call link's platform.
func recorderFor(cfg *Config, callURL string) string {
	if meetURL(callURL) != "" {
		return cfg.MeetRecorderURL
	}
	return cfg.JitsiRecorderURL
}

// postRecording asks the recorder to join; network errors and 5xx are retried.
func postRecording(ctx context.Context, cfg *Config, j *job) error {
	base := recorderFor(cfg, j.URL)
	if base == "" {
		return fmt.Errorf("no recorder configured for this platform: %w", errPermanent)
	}
	body, err := json.Marshal(map[string]any{
		"id":             j.ID,
		"url":            j.URL,
		"callback_url":   cfg.PublicURL + "/events",
		"display_name":   cfg.BotDisplayName,
		"join_timeout_s": int(cfg.JoinTimeout.Seconds()),
		"max_duration_s": int((j.End.Sub(j.Start) + cfg.Overrun).Seconds()),
		"empty_grace_s":  int(cfg.EmptyGrace.Seconds()),
	})
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err = postOnce(ctx, base+"/recordings", cfg.RecorderSecret, body)
		if err == nil || errors.Is(err, errPermanent) || attempt >= len(recorderRetries) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(recorderRetries[attempt]):
		}
	}
}

func postOnce(ctx context.Context, u, secret string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-recorder-signature", sign(secret, body))
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode < 300: // 202 started, 200 already exists
		return nil
	case resp.StatusCode >= 500:
		return fmt.Errorf("recorder status %d", resp.StatusCode)
	default:
		return fmt.Errorf("recorder status %d: %w", resp.StatusCode, errPermanent)
	}
}

// recEvent is the part of a recorder event (architecture.md §3.5) read here.
type recEvent struct {
	Event     string  `json:"event"`
	ID        string  `json:"id"`
	Error     string  `json:"error"`
	DurationS float64 `json:"duration_s"`
}

// handleEvents is POST /events from the recorders. Events are idempotent on
// (id, event): a repeat is acknowledged without side effects.
func handleEvents(w http.ResponseWriter, r *http.Request, cfg *Config) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if !hmac.Equal([]byte(r.Header.Get("x-recorder-signature")), []byte(sign(cfg.RecorderSecret, body))) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var ev recEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.Event == "" || ev.ID == "" {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}

	jobsMu.Lock()
	defer jobsMu.Unlock()
	var j *job
	if jobIDRe.MatchString(ev.ID) {
		j, _ = readJob(cfg.DataDir, ev.ID)
	}
	if j == nil || j.State == stateScheduled {
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	if slices.Contains(j.Events, ev.Event) {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch ev.Event {
	case "recording.waiting_admission", "recording.started":
	case "recording.finished":
		j.State = stateFinished
	case "recording.failed":
		j.State, j.Error = stateFailed, ev.Error
	default:
		slog.Warn("unknown recorder event ignored", "job", j.ID, "event", ev.Event)
		w.WriteHeader(http.StatusOK)
		return
	}
	j.Events = append(j.Events, ev.Event)
	// State first, side effects after: a failed write is retried by the
	// recorder and must not have sent anything yet.
	if err := writeJob(cfg.DataDir, j); err != nil {
		slog.Error("write job failed", "job", j.ID, "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	slog.Info("recorder event", "job", j.ID, "event", ev.Event)

	switch ev.Event {
	case "recording.waiting_admission":
		notifier.mailLobby(j.ID, j.Notify, j.Title)
	case "recording.finished":
		if time.Duration(ev.DurationS*float64(time.Second)) < cfg.MinRecording {
			notifier.mailTooShort(j.ID, j.Notify, j.Title)
		} else {
			handOff(cfg, j, body)
		}
	case "recording.failed": // a partial recording is never handed off
		reason := ev.Error
		if reason == "" {
			reason = "unknown error"
		}
		notifier.mailFailed(j.ID, j.Notify, j.Title, reason)
	}
	w.WriteHeader(http.StatusOK)
}
