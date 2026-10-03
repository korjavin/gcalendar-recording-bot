package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// useFakeSMTP routes every notification to a fake SMTP server.
func useFakeSMTP(t *testing.T) *fakeSMTP {
	f := newFakeSMTP(t, 0)
	old := notifier
	notifier = f.mailer(t)
	t.Cleanup(func() { notifier = old })
	return f
}

// none asserts that no e-mail goes out.
func (f *fakeSMTP) none(t *testing.T) {
	t.Helper()
	select {
	case m := <-f.got:
		t.Errorf("unexpected e-mail to %v:\n%s", m.to, m.data)
	case <-time.After(300 * time.Millisecond):
	}
}

func (f *fakeSMTP) subject(t *testing.T, want string) {
	t.Helper()
	m := f.wait(t)
	if !strings.Contains(m.data, "Subject: "+want+"\r\n") || !slices.Equal(m.to, []string{"RCPT TO:<alice@example.com>", "RCPT TO:<bob@example.com>"}) {
		t.Errorf("e-mail to %v, want subject %q:\n%s", m.to, want, m.data)
	}
}

type recReq struct {
	sig  string
	body map[string]any
}

// fakeRecorder answers POST /recordings with the queued statuses, then 202.
type fakeRecorder struct {
	*httptest.Server
	mu       sync.Mutex
	statuses []int
	reqs     []recReq
}

func newFakeRecorder(t *testing.T, statuses ...int) *fakeRecorder {
	f := &fakeRecorder{statuses: statuses}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/recordings" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reqs = append(f.reqs, recReq{r.Header.Get("x-recorder-signature"), body})
		if r.Header.Get("x-recorder-signature") != sign("rec-secret", raw) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		status := http.StatusAccepted
		if len(f.statuses) > 0 {
			status, f.statuses = f.statuses[0], f.statuses[1:]
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRecorder) requests() []recReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func schedulerSetup(t *testing.T) (*Config, *fakeRecorder, *fakeRecorder, *fakeSMTP) {
	cfg := testConfig(t)
	cfg.RecorderSecret = "rec-secret"
	cfg.JoinLead, cfg.JoinTimeout = 90*time.Second, 1200*time.Second
	cfg.Overrun, cfg.EmptyGrace, cfg.MinRecording = 1800*time.Second, 60*time.Second, 60*time.Second
	jitsi, meet := newFakeRecorder(t), newFakeRecorder(t)
	cfg.JitsiRecorderURL, cfg.MeetRecorderURL = jitsi.URL, meet.URL
	old := recorderRetries
	recorderRetries = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { recorderRetries = old })
	return cfg, jitsi, meet, useFakeSMTP(t)
}

func putJob(t *testing.T, cfg *Config, id, state, url string, start time.Time) {
	t.Helper()
	j := &job{ID: id, State: state, Title: "Weekly", URL: url, Start: start, End: start.Add(time.Hour),
		Notify: []string{"alice@example.com", "bob@example.com"}}
	if err := writeJob(cfg.DataDir, j); err != nil {
		t.Fatal(err)
	}
}

func jobState(t *testing.T, cfg *Config, id string) string {
	t.Helper()
	j, err := readJob(cfg.DataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	return j.State
}

const (
	meetLink  = "https://meet.google.com/abc-defg-hij"
	jitsiLink = "https://jitsi.example.com/Room"
)

func TestStartDueTimingAndRequest(t *testing.T) {
	cfg, jitsi, meet, smtp := schedulerSetup(t)
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	putJob(t, cfg, "cal-0000000000000001", stateScheduled, meetLink, now.Add(89*time.Second))   // inside the lead
	putJob(t, cfg, "cal-0000000000000002", stateScheduled, jitsiLink, now.Add(-30*time.Minute)) // running, end ahead
	putJob(t, cfg, "cal-0000000000000003", stateScheduled, meetLink, now.Add(91*time.Second))   // too early
	putJob(t, cfg, "cal-0000000000000004", stateScheduled, meetLink, now.Add(-time.Hour))       // ended now
	putJob(t, cfg, "cal-0000000000000005", stateStarting, jitsiLink, now)                       // interrupted start
	putJob(t, cfg, "cal-0000000000000006", stateStarted, jitsiLink, now)                        // already running

	startDue(context.Background(), cfg, now)

	want := map[string]string{"1": stateStarted, "2": stateStarted, "3": stateScheduled,
		"4": stateScheduled, "5": stateStarted, "6": stateStarted}
	for n, st := range want {
		if got := jobState(t, cfg, "cal-000000000000000"+n); got != st {
			t.Errorf("job %s state = %q, want %q", n, got, st)
		}
	}
	mr, jr := meet.requests(), jitsi.requests()
	if len(mr) != 1 || len(jr) != 2 {
		t.Fatalf("requests: meet %d, jitsi %d; want 1 and 2", len(mr), len(jr))
	}
	b := mr[0].body
	if b["id"] != "cal-0000000000000001" || b["url"] != meetLink || b["callback_url"] != "https://bot.example.com/events" ||
		b["display_name"] != "NoteTaker" || b["join_timeout_s"] != 1200.0 || b["max_duration_s"] != 5400.0 || b["empty_grace_s"] != 60.0 {
		t.Errorf("request body = %v", b)
	}
	if !strings.HasPrefix(mr[0].sig, "sha256=") {
		t.Errorf("signature = %q", mr[0].sig)
	}
	smtp.none(t)

	startDue(context.Background(), cfg, now)
	if len(meet.requests())+len(jitsi.requests()) != 3 {
		t.Error("a started job was posted again")
	}
}

func TestStartRetries(t *testing.T) {
	cfg, _, meet, smtp := schedulerSetup(t)
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	meet.statuses = []int{503, 502}
	putJob(t, cfg, "cal-0000000000000001", stateScheduled, meetLink, now)
	startDue(context.Background(), cfg, now)
	if st := jobState(t, cfg, "cal-0000000000000001"); st != stateStarted || len(meet.requests()) != 3 {
		t.Errorf("after two 5xx: state %q, %d requests; want started after 3", st, len(meet.requests()))
	}
	smtp.none(t)
}

func TestStartFails(t *testing.T) {
	for name, tc := range map[string]struct {
		statuses []int
		requests int
	}{
		"5xx exhausts retries": {[]int{500, 500, 500}, 3},
		"4xx is not retried":   {[]int{422}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _, meet, smtp := schedulerSetup(t)
			now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
			meet.statuses = tc.statuses
			putJob(t, cfg, "cal-0000000000000001", stateScheduled, meetLink, now)
			startDue(context.Background(), cfg, now)
			if st := jobState(t, cfg, "cal-0000000000000001"); st != stateFailed || len(meet.requests()) != tc.requests {
				t.Errorf("state %q, %d requests; want failed after %d", st, len(meet.requests()), tc.requests)
			}
			smtp.subject(t, "Recording failed: Weekly (the recorder could not be reached)")
		})
	}
}

func postEvent(cfg *Config, body string, sig string) int {
	if sig == "" {
		sig = sign(cfg.RecorderSecret, []byte(body))
	}
	req := httptest.NewRequest("POST", "/events", strings.NewReader(body))
	req.Header.Set("x-recorder-signature", sig)
	rec := httptest.NewRecorder()
	webMux(cfg).ServeHTTP(rec, req)
	return rec.Code
}

func TestEvents(t *testing.T) {
	const id = "cal-0000000000000001"
	ev := func(event, extra string) string {
		return `{"event":"` + event + `","id":"` + id + `","source":"meet"` + extra + `}`
	}
	tests := []struct {
		name, body, state, subject string
		handedOff                  bool
	}{
		{"waiting_admission", ev("recording.waiting_admission", ""), stateStarted, "NoteTaker is waiting in the lobby: Weekly", false},
		{"started", ev("recording.started", ""), stateStarted, "", false},
		{"finished", ev("recording.finished", `,"duration_s":60.5`), stateFinished, "", true},
		{"finished too short", ev("recording.finished", `,"duration_s":59.9`), stateFinished, "Recording too short: Weekly", false},
		{"failed partial", ev("recording.failed", `,"error":"interrupted","artifacts":[{"kind":"audio","path":"/data/meet/x/audio.wav"}]`),
			stateFailed, "Recording failed: Weekly (interrupted)", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _, smtp := schedulerSetup(t)
			var handed []string
			old := handOff
			handOff = func(_ *Config, j *job, _ []byte) { handed = append(handed, j.ID) }
			t.Cleanup(func() { handOff = old })
			putJob(t, cfg, id, stateStarted, meetLink, time.Now())

			for range 2 { // the repeat is acknowledged without side effects
				if code := postEvent(cfg, tc.body, ""); code != 200 {
					t.Fatalf("status %d", code)
				}
			}
			if st := jobState(t, cfg, id); st != tc.state {
				t.Errorf("state %q, want %q", st, tc.state)
			}
			if tc.subject != "" {
				smtp.subject(t, tc.subject)
			}
			smtp.none(t)
			if want := tc.handedOff; (len(handed) == 1) != want || len(handed) > 1 {
				t.Errorf("handed off %v, want once=%v", handed, want)
			}
		})
	}
}

func TestEventsRejected(t *testing.T) {
	cfg, _, _, smtp := schedulerSetup(t)
	putJob(t, cfg, "cal-0000000000000001", stateStarted, meetLink, time.Now())
	putJob(t, cfg, "cal-0000000000000002", stateScheduled, meetLink, time.Now())
	lobby := func(id string) string { return `{"event":"recording.waiting_admission","id":"` + id + `"}` }

	if code := postEvent(cfg, lobby("cal-0000000000000001"), "sha256=00"); code != 401 {
		t.Errorf("bad signature: %d, want 401", code)
	}
	if code := postEvent(cfg, lobby("cal-0000000000000001"), sign("other", []byte(lobby("cal-0000000000000001")))); code != 401 {
		t.Errorf("wrong secret: %d, want 401", code)
	}
	for _, id := range []string{"cal-00000000000000ff", "cal-0000000000000002", "../../connections/x"} {
		if code := postEvent(cfg, lobby(id), ""); code != 404 {
			t.Errorf("job %q: %d, want 404", id, code)
		}
	}
	if code := postEvent(cfg, `{"id":"cal-0000000000000001"}`, ""); code != 400 {
		t.Errorf("no event: %d, want 400", code)
	}
	smtp.none(t)
	if st := jobState(t, cfg, "cal-0000000000000002"); st != stateScheduled {
		t.Errorf("scheduled job touched: %q", st)
	}
}
