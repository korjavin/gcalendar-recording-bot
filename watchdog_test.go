package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// watchRecorder answers GET /recordings/<id> with status and body, and
// counts the correctly signed requests.
func watchRecorder(t *testing.T, cfg *Config, id string, status *atomic.Int32, body *atomic.Value) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/recordings/"+id || r.Header.Get("x-recorder-signature") != sign(cfg.RecorderSecret, nil) {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
		w.Write([]byte(body.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func putWatchedJob(t *testing.T, cfg *Config, id, state string, deadline time.Time) {
	t.Helper()
	start := deadline.Add(-3 * time.Hour)
	j := &job{ID: id, State: state, Title: "Weekly", URL: meetLink, Start: start, End: start.Add(time.Hour),
		Notify: []string{"alice@example.com", "bob@example.com"}, Deadline: deadline}
	if err := writeJob(cfg.DataDir, j); err != nil {
		t.Fatal(err)
	}
}

func TestWatchdog(t *testing.T) {
	const id = "cal-0000000000000001"
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	finished := `{"id":"` + id + `","state":"finished","error":null,"started_at":"2026-10-03T15:01:00Z","ended_at":"2026-10-03T16:30:00Z",` +
		`"duration_s":5340,"participants":["Alice"],"artifacts":[{"kind":"audio","path":"/data/meet/` + id + `/audio.wav"}]}`
	tests := []struct {
		name       string
		state      string // the job's state before the checks
		status     int    // 0: the recorder is unreachable
		body       string
		checks     int
		wantState  string
		subject    string
		handedOff  bool
		wantMisses int
	}{
		{"still running", stateStarted, 200, `{"id":"` + id + `","state":"recording"}`, 1, stateStarted, "", false, 0},
		{"recovered finished", stateStarted, 200, finished, 1, stateFinished, "", true, 0},
		{"recovered failed", stateStarted, 200, `{"id":"` + id + `","state":"failed","error":"not_admitted"}`, 1,
			stateFailed, "Recording failed: Weekly (not_admitted)", false, 0},
		{"404 twice", stateStarted, 404, `{"error":"not found"}`, 2, stateStarted, "", false, 2},
		{"404 three times", stateStarted, 404, `{"error":"not found"}`, 3, stateFailed, "Recording failed: Weekly (lost)", false, 3},
		{"unreachable three times", stateStarted, 0, "", 3, stateFailed, "Recording failed: Weekly (lost)", false, 3},
		// A start interrupted by a crash, its meeting long over: the
		// scheduler leaves it to the watchdog.
		{"stale starting", stateStarting, 404, `{"error":"not found"}`, 3, stateFailed, "Recording failed: Weekly (lost)", false, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _, smtp := schedulerSetup(t)
			tr := newFakeTranscriber(t, cfg)
			var status atomic.Int32
			var body atomic.Value
			status.Store(int32(tc.status))
			body.Store(tc.body)
			srv, hits := watchRecorder(t, cfg, id, &status, &body)
			cfg.MeetRecorderURL = srv.URL
			if tc.status == 0 {
				srv.Close()
			}
			putWatchedJob(t, cfg, id, tc.state, now)

			for i := range tc.checks {
				watchOverdue(context.Background(), cfg, now.Add(time.Duration(i)*time.Minute))
			}
			if tc.status != 0 && int(hits.Load()) != tc.checks {
				t.Errorf("%d signed GETs, want %d", hits.Load(), tc.checks)
			}
			j, err := readJob(cfg.DataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if j.State != tc.wantState || j.Misses != tc.wantMisses {
				t.Errorf("state %q misses %d, want %q %d", j.State, j.Misses, tc.wantState, tc.wantMisses)
			}
			if tc.subject != "" {
				smtp.subject(t, tc.subject)
			}
			if tc.handedOff {
				waitHandedOff(t, cfg, id)
				var w webhook
				json.Unmarshal([]byte(tr.requests()[0].body), &w)
				if w.AudioPath != "/data/meet/"+id+"/audio.wav" || w.DurationS != 5340 || w.StartedAt != "2026-10-03T15:01:00Z" {
					t.Errorf("webhook from the job record = %+v", w)
				}
			}
			smtp.none(t)

			// A terminal job is never checked again; a running one again
			// after watchdogDelay, not before.
			before := hits.Load()
			switch {
			case tc.wantState != stateStarted:
				watchOverdue(context.Background(), cfg, now.Add(time.Hour))
			case tc.name == "still running":
				watchOverdue(context.Background(), cfg, now.Add(watchdogDelay-time.Second))
				if hits.Load() != before {
					t.Error("checked again before the new deadline")
				}
				watchOverdue(context.Background(), cfg, now.Add(watchdogDelay))
				before++
			}
			if hits.Load() != before {
				t.Errorf("%d GETs, want %d", hits.Load(), before)
			}
		})
	}
}

// A miss is reset by an answer: only misses in a row make a job lost.
func TestWatchdogMissesInARow(t *testing.T) {
	const id = "cal-0000000000000001"
	cfg, _, _, smtp := schedulerSetup(t)
	var status atomic.Int32
	var body atomic.Value
	body.Store(`{"id":"` + id + `","state":"recording"}`)
	srv, _ := watchRecorder(t, cfg, id, &status, &body)
	cfg.MeetRecorderURL = srv.URL
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	putWatchedJob(t, cfg, id, stateStarted, now)

	for i, st := range []int32{404, 503, 200} {
		status.Store(st)
		watchOverdue(context.Background(), cfg, now.Add(time.Duration(i)*time.Minute))
	}
	status.Store(404)
	watchOverdue(context.Background(), cfg, now.Add(time.Hour))
	if j, _ := readJob(cfg.DataDir, id); j.State != stateStarted || j.Misses != 1 {
		t.Errorf("state %q misses %d, want started 1", j.State, j.Misses)
	}
	smtp.none(t)
}

func TestClaimSetsDeadline(t *testing.T) {
	cfg, _, _, _ := schedulerSetup(t)
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	putJob(t, cfg, "cal-0000000000000001", stateScheduled, meetLink, now)
	claimDue(cfg, now)
	j, _ := readJob(cfg.DataDir, "cal-0000000000000001")
	// join_timeout_s 1200 + max_duration_s (1 h meeting + 1800 s overrun) + 10 min
	if want := now.Add(1200*time.Second + 90*time.Minute + 10*time.Minute); !j.Deadline.Equal(want) {
		t.Errorf("deadline %v, want %v", j.Deadline, want)
	}
}
