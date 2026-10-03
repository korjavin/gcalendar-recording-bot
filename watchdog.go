package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// watchdogDelay is the slack after join_timeout_s + max_duration_s before a
// job without a terminal event is checked, and the pause between checks of
// one the recorder still runs.
const watchdogDelay = 10 * time.Minute

// watchdogMisses checks in a row that find no job (404 or unreachable) make
// the job failed with error "lost".
const watchdogMisses = 3

var errNoRecording = errors.New("recording not found")

// runWatchdog checks overdue jobs every minute until ctx ends. A job that
// was running before a restart stays "started" (or "starting") and is
// watched like any other.
func runWatchdog(ctx context.Context, cfg *Config) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		watchOverdue(ctx, cfg, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// watchOverdue asks the recorder about every job past its deadline.
// ponytail: checks run one after another; parallel if many recorders hang.
func watchOverdue(ctx context.Context, cfg *Config, now time.Time) {
	for _, j := range overdueJobs(cfg, now) {
		rec, err := getRecording(ctx, cfg, j)
		watchResult(cfg, j.ID, rec, err, now)
	}
}

// overdueJobs lists the jobs still starting or started after their deadline.
func overdueJobs(cfg *Config, now time.Time) []*job {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	ents, _ := os.ReadDir(filepath.Join(cfg.DataDir, "jobs"))
	var due []*job
	for _, e := range ents {
		j, err := readJob(cfg.DataDir, e.Name())
		if err == nil && (j.State == stateStarting || j.State == stateStarted) && !now.Before(j.Deadline) {
			due = append(due, j)
		}
	}
	return due
}

// recording is the part of the recorder's job record (architecture.md §3.6)
// read here; its timing, participant and artifact fields are named as in the
// events.
type recording struct {
	State string `json:"state"`
	recEvent
}

// getRecording is GET /recordings/{id}, signed over the empty body. A 404
// is errNoRecording; any other non-200 or network error is "unreachable".
func getRecording(ctx context.Context, cfg *Config, j *job) (*recording, error) {
	base := recorderFor(cfg, j.URL)
	if base == "" {
		return nil, errors.New("no recorder configured for this platform")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/recordings/"+j.ID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-recorder-signature", sign(cfg.RecorderSecret, nil))
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errNoRecording
	default:
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var rec recording
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rec); err != nil {
		return nil, fmt.Errorf("job record: %w", err)
	}
	return &rec, nil
}

// watchResult acts on one check, under jobsMu, unless an event has moved
// the job on meanwhile: a finished or failed record is applied as the lost
// event; a running one is checked again after watchdogDelay; a miss counts
// towards "lost".
func watchResult(cfg *Config, id string, rec *recording, err error, now time.Time) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	j, rerr := readJob(cfg.DataDir, id)
	if rerr != nil || (j.State != stateStarting && j.State != stateStarted) {
		return
	}
	switch {
	case err == nil && (rec.State == stateFinished || rec.State == stateFailed):
		slog.Warn("watchdog: recorder event was lost; taking the job record", "job", id, "state", rec.State)
		ev := rec.recEvent
		ev.Event, ev.ID = "recording."+rec.State, id
		applyEvent(cfg, j, &ev) // a failed save is retried on the next tick
		return
	case err == nil:
		slog.Info("watchdog: still running", "job", id, "state", rec.State)
		j.Deadline, j.Misses = now.Add(watchdogDelay), 0
	default:
		j.Misses++
		slog.Warn("watchdog: check failed", "job", id, "misses", j.Misses, "err", err)
		if j.Misses >= watchdogMisses {
			applyEvent(cfg, j, &recEvent{Event: "recording.failed", ID: id, Error: "lost"})
			return
		}
	}
	if err := writeJob(cfg.DataDir, j); err != nil {
		slog.Error("write job failed", "job", id, "err", err)
	}
}
