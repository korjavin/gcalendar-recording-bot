package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const jitsiBase = "https://jitsi.example.com"

func ev(mod func(*calEvent)) *calEvent {
	e := &calEvent{ICalUID: "uid-1@google.com", Status: "confirmed", Summary: "Weekly #note"}
	e.Start.DateTime = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	e.End.DateTime = e.Start.DateTime.Add(time.Hour)
	if mod != nil {
		mod(e)
	}
	return e
}

func withVideo(uri string) func(*calEvent) {
	return func(e *calEvent) {
		e.ConferenceData.EntryPoints = append(e.ConferenceData.EntryPoints,
			entryPoint{"phone", "tel:+1-555-0100"}, entryPoint{"video", uri})
	}
}

func TestCandidateURL(t *testing.T) {
	meet := "https://meet.google.com/abc-defg-hij"
	tests := []struct {
		name string
		ev   *calEvent
		want string
	}{
		{"no tag", ev(func(e *calEvent) { e.Summary = "Weekly"; e.HangoutLink = meet }), ""},
		{"#notes", ev(func(e *calEvent) { e.Summary = "Weekly #notes"; e.HangoutLink = meet }), ""},
		{"#notebook", ev(func(e *calEvent) { e.Summary = "#notebook review"; e.HangoutLink = meet }), ""},
		{"tag inside a word", ev(func(e *calEvent) { e.Summary = "a#note"; e.HangoutLink = meet }), ""},
		{"double hash", ev(func(e *calEvent) { e.Summary = "##note"; e.HangoutLink = meet }), ""},
		{"tag at title start", ev(func(e *calEvent) { e.Summary = "#note: weekly"; e.HangoutLink = meet }), meet},
		{"tag in description, HTML, other case", ev(func(e *calEvent) {
			e.Summary = "Weekly"
			e.Description = "Agenda<br><b>#NOTE</b>&nbsp;please"
			e.HangoutLink = meet
		}), meet},
		{"escaped tag inside a word in description", ev(func(e *calEvent) {
			e.Summary = "Weekly"
			e.Description = "x&#35;note"
			e.HangoutLink = meet
		}), ""},
		{"tag in location", ev(func(e *calEvent) { e.Summary = "Weekly"; e.Location = "Room 1 (#Note)"; e.HangoutLink = meet }), meet},
		{"cancelled", ev(func(e *calEvent) { e.Status = "cancelled"; e.HangoutLink = meet }), ""},
		{"no link", ev(nil), ""},
		{"all-day", ev(func(e *calEvent) { e.Start.DateTime = time.Time{}; e.HangoutLink = meet }), ""},
		{"meet via conferenceData", ev(withVideo(meet + "?authuser=0")), meet},
		{"hangoutLink", ev(func(e *calEvent) { e.HangoutLink = meet }), meet},
		{"foreign meet-like host", ev(func(e *calEvent) { e.HangoutLink = "https://meet.google.com.evil.example/abc" }), ""},
		{"jitsi in description", ev(func(e *calEvent) {
			e.Description = `Join: <a href="https://jitsi.example.com/TeamSync?x=1&amp;y=2">here</a>.`
		}), "https://jitsi.example.com/TeamSync?x=1&y=2"},
		{"jitsi in location", ev(func(e *calEvent) { e.Location = "https://jitsi.example.com/Room." }), "https://jitsi.example.com/Room"},
		{"foreign jitsi host", ev(func(e *calEvent) {
			e.Description = "https://meet.jit.si/Room https://jitsi.example.com.evil.example/Room"
		}), ""},
		{"jitsi base without room", ev(func(e *calEvent) { e.Description = "https://jitsi.example.com/" }), ""},
		{"both links: jitsi wins", ev(func(e *calEvent) {
			e.Description = "https://jitsi.example.com/Room"
			e.HangoutLink = meet
		}), "https://jitsi.example.com/Room"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := candidateURL(tt.ev, jitsiBase); got != tt.want {
				t.Errorf("candidateURL = %q, want %q", got, tt.want)
			}
		})
	}
	if got := candidateURL(ev(func(e *calEvent) { e.Description = "https://jitsi.example.com/Room" }), ""); got != "" {
		t.Errorf("jitsi without JITSI_BASE_URL = %q, want empty", got)
	}
}

func TestJobID(t *testing.T) {
	start := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	berlin := start.In(time.FixedZone("CEST", 2*3600))
	id := jobID("uid-1@google.com", start)
	if len(id) != len("cal-")+16 || id[:4] != "cal-" {
		t.Fatalf("id = %q", id)
	}
	if jobID("uid-1@google.com", berlin) != id {
		t.Error("id differs across time zones of the same instant (two attendees' copies)")
	}
	if jobID("uid-1@google.com", start.Add(7*24*time.Hour)) == id {
		t.Error("recurrences share an id")
	}
	if jobID("uid-2@google.com", start) == id {
		t.Error("distinct events share an id")
	}
}

// fakeCalendar serves the token endpoint and events.list. Each refresh token
// maps to a calendar; "revoked" answers invalid_grant. Pages hold one event.
type fakeCalendar struct {
	mu     sync.Mutex
	events map[string][]map[string]any // by access token
	fail   map[string]bool             // access tokens whose events.list returns 500
	deny   map[string]bool             // access tokens whose events.list returns 403 accessNotConfigured
	pages  int
}

func newFakeCalendar(t *testing.T) *fakeCalendar {
	f := &fakeCalendar{events: map[string][]map[string]any{}, fail: map[string]bool{}, deny: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		rt := r.Form.Get("refresh_token")
		if r.Form.Get("grant_type") != "refresh_token" || rt == "revoked" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "at-" + rt})
	})
	mux.HandleFunc("GET /calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pages++
		q := r.URL.Query()
		if q.Get("singleEvents") != "true" || q.Get("orderBy") != "startTime" || q.Get("timeMin") == "" || q.Get("timeMax") == "" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		at := r.Header.Get("Authorization")[len("Bearer "):]
		if f.fail[at] {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if f.deny[at] {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"code":403,"message":"Google Calendar API has not been used in project 123 before or it is disabled.",` +
				`"errors":[{"message":"Google Calendar API has not been used","domain":"usageLimits","reason":"accessNotConfigured"}],"status":"PERMISSION_DENIED"}}`))
			return
		}
		evs := f.events[at]
		i, _ := strconv.Atoi(q.Get("pageToken"))
		resp := map[string]any{"items": []any{}}
		if i < len(evs) {
			resp["items"] = evs[i : i+1]
		}
		if i+1 < len(evs) {
			resp["nextPageToken"] = strconv.Itoa(i + 1)
		}
		json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	oldTok, oldCal := googleTokenURL, googleCalendarURL
	googleTokenURL, googleCalendarURL = srv.URL+"/token", srv.URL
	t.Cleanup(func() { googleTokenURL, googleCalendarURL = oldTok, oldCal })
	return f
}

func (f *fakeCalendar) set(refresh string, evs ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events["at-"+refresh] = evs
}

func apiEvent(uid string, start time.Time, mod func(map[string]any)) map[string]any {
	e := map[string]any{
		"iCalUID": uid, "status": "confirmed", "summary": "Sync " + uid,
		"description": "Agenda #note",
		"hangoutLink": "https://meet.google.com/abc-defg-hij",
		"start":       map[string]string{"dateTime": start.Format(time.RFC3339)},
		"end":         map[string]string{"dateTime": start.Add(time.Hour).Format(time.RFC3339)},
	}
	if mod != nil {
		mod(e)
	}
	return e
}

func plannerSetup(t *testing.T) (*Config, *fakeCalendar) {
	cfg := testConfig(t)
	cfg.JitsiBaseURL = jitsiBase
	return cfg, newFakeCalendar(t)
}

func connect(t *testing.T, cfg *Config, email, refresh string) {
	t.Helper()
	if err := saveConnection(cfg.DataDir, cfg.TokenKey, email, refresh, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func jobs(t *testing.T, cfg *Config) map[string]job {
	t.Helper()
	out := map[string]job{}
	ents, _ := os.ReadDir(filepath.Join(cfg.DataDir, "jobs"))
	for _, e := range ents {
		data, err := os.ReadFile(jobPath(cfg.DataDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var j job
		if err := json.Unmarshal(data, &j); err != nil {
			t.Fatal(err)
		}
		out[j.ID] = j
	}
	return out
}

func poll(t *testing.T, cfg *Config, now time.Time) {
	t.Helper()
	if err := pollOnce(context.Background(), cfg, now); err != nil {
		t.Fatal(err)
	}
}

func TestPollMergesPaginatesAndReconciles(t *testing.T) {
	cfg, f := plannerSetup(t)
	now := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	t1, t2 := now.Add(2*time.Hour), now.Add(4*time.Hour)
	connect(t, cfg, "alice@example.com", "rt-alice")
	connect(t, cfg, "bob@example.com", "rt-bob")

	// Bob sees the shared meeting in his own time zone; Alice's calendar has
	// three pages, one of which is untagged.
	shared := apiEvent("shared@google.com", t1, nil)
	f.set("rt-alice", shared,
		apiEvent("solo@google.com", t2, nil),
		apiEvent("other@google.com", t2, func(e map[string]any) { e["description"] = "Agenda" }))
	f.set("rt-bob", apiEvent("shared@google.com", t1.In(time.FixedZone("X", -5*3600)), nil))

	poll(t, cfg, now)
	got := jobs(t, cfg)
	sharedID, soloID := jobID("shared@google.com", t1), jobID("solo@google.com", t2)
	if len(got) != 2 {
		t.Fatalf("jobs = %v, want shared + solo", got)
	}
	if j := got[sharedID]; j.State != "scheduled" || !slices.Equal(j.Notify, []string{"alice@example.com", "bob@example.com"}) ||
		j.URL != "https://meet.google.com/abc-defg-hij" || j.Title != "Sync shared@google.com" || !j.Start.Equal(t1) || !j.End.Equal(t1.Add(time.Hour)) {
		t.Errorf("shared job = %+v", j)
	}
	if j := got[soloID]; !slices.Equal(j.Notify, []string{"alice@example.com"}) {
		t.Errorf("solo notify = %v", j.Notify)
	}
	if f.pages != 4 {
		t.Errorf("events.list pages fetched = %d, want 4 (3 alice + 1 bob)", f.pages)
	}

	// The solo meeting moves (new id), Bob's copy comes back untagged
	// (Alice's copy is still tagged), a started job stays untouched.
	started := job{ID: "cal-started0000000", State: "recording", Title: "x"}
	data, _ := json.Marshal(started)
	writeFileAtomic(jobPath(cfg.DataDir, started.ID), data)
	f.set("rt-alice", shared, apiEvent("solo@google.com", t2.Add(30*time.Minute), nil))
	f.set("rt-bob", apiEvent("shared@google.com", t1, func(e map[string]any) {
		e["description"] = "Agenda"
	}))
	poll(t, cfg, now)
	got = jobs(t, cfg)
	movedID := jobID("solo@google.com", t2.Add(30*time.Minute))
	if _, ok := got[soloID]; ok {
		t.Error("moved occurrence's old job not dropped")
	}
	if _, ok := got[movedID]; !ok {
		t.Error("moved occurrence not scheduled")
	}
	if j := got[sharedID]; !slices.Equal(j.Notify, []string{"alice@example.com"}) {
		t.Errorf("shared notify after bob untagged = %v", j.Notify)
	}
	if j := got[started.ID]; j.State != "recording" {
		t.Errorf("started job touched: %+v", j)
	}

	// Cancelled everywhere → dropped; the started job survives.
	f.set("rt-alice", apiEvent("shared@google.com", t1, func(e map[string]any) { e["status"] = "cancelled" }))
	f.set("rt-bob")
	poll(t, cfg, now)
	got = jobs(t, cfg)
	if len(got) != 1 || got[started.ID].State != "recording" {
		t.Errorf("after cancel jobs = %v, want only the started one", got)
	}
}

func TestPollPartialFailureKeepsJobs(t *testing.T) {
	cfg, f := plannerSetup(t)
	now := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	t1 := now.Add(time.Hour)
	connect(t, cfg, "alice@example.com", "rt-alice")
	connect(t, cfg, "bob@example.com", "rt-bob")
	f.set("rt-alice", apiEvent("a@google.com", t1, nil))
	f.set("rt-bob", apiEvent("a@google.com", t1, nil), apiEvent("b@google.com", t1, nil))
	poll(t, cfg, now)

	f.mu.Lock()
	f.fail["at-rt-bob"] = true
	f.mu.Unlock()
	poll(t, cfg, now)
	got := jobs(t, cfg)
	if len(got) != 2 {
		t.Fatalf("jobs after bob's poll failed = %v, want both kept", got)
	}
	if j := got[jobID("a@google.com", t1)]; !slices.Equal(j.Notify, []string{"alice@example.com", "bob@example.com"}) {
		t.Errorf("notify after partial poll = %v, want bob kept", j.Notify)
	}
}

func TestPollInvalidGrantDropsConnection(t *testing.T) {
	cfg, f := plannerSetup(t)
	smtp := useFakeSMTP(t)
	now := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	connect(t, cfg, "carol@example.com", "revoked")
	connect(t, cfg, "alice@example.com", "rt-alice")
	f.set("rt-alice")

	// A job only Carol's calendar produced is dropped with her connection.
	stale := job{ID: jobID("c@google.com", now.Add(time.Hour)), State: "scheduled", Notify: []string{"carol@example.com"}}
	data, _ := json.Marshal(stale)
	writeFileAtomic(jobPath(cfg.DataDir, stale.ID), data)

	poll(t, cfg, now)
	if _, err := os.Stat(connectionPath(cfg.DataDir, "carol@example.com")); !os.IsNotExist(err) {
		t.Errorf("revoked connection still stored: %v", err)
	}
	if _, err := os.Stat(connectionPath(cfg.DataDir, "alice@example.com")); err != nil {
		t.Errorf("healthy connection removed: %v", err)
	}
	if m := smtp.wait(t); !slices.Equal(m.to, []string{"RCPT TO:<carol@example.com>"}) || !strings.Contains(m.data, "Subject: Calendar disconnected") {
		t.Errorf("disconnect notice to %v:\n%s", m.to, m.data)
	}
	if len(jobs(t, cfg)) != 0 {
		t.Error("revoked connection's job not dropped")
	}
	poll(t, cfg, now)
	smtp.none(t)
}

// A reconnect that lands while the old, revoked token is being refreshed must
// survive: only the polled version of the connection file is dropped.
func TestDropConnectionKeepsReplacedConnection(t *testing.T) {
	cfg := testConfig(t)
	connect(t, cfg, "alice@example.com", "revoked")
	path := connectionPath(cfg.DataDir, "alice@example.com")
	polled, _ := os.ReadFile(path)
	connect(t, cfg, "alice@example.com", "rt-new")
	if dropConnection(path, polled) {
		t.Fatal("dropped a connection replaced after the poll read it")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("replaced connection removed: %v", err)
	}
	cur, _ := os.ReadFile(path)
	if !dropConnection(path, cur) {
		t.Fatal("unchanged connection not dropped")
	}
}

// captureLogs routes slog to a buffer for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestPollLogs(t *testing.T) {
	cfg, f := plannerSetup(t)
	now := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	t1 := now.Add(time.Hour)
	connect(t, cfg, "alice@example.com", "rt-alice")
	connect(t, cfg, "bob@example.com", "rt-bob")
	f.set("rt-alice", apiEvent("a@google.com", t1, nil),
		apiEvent("b@google.com", t1, func(e map[string]any) { e["description"] = "Agenda" }))
	f.mu.Lock()
	f.deny["at-rt-bob"] = true
	f.mu.Unlock()
	alice, bobFull := emailHash("alice@example.com")[:12], emailHash("bob@example.com")
	id := jobID("a@google.com", t1)

	logs := captureLogs(t)
	poll(t, cfg, now)
	out := logs.String()
	for _, want := range []string{
		`msg="connection polled" connection=` + alice + ` events=2 candidates=1`,
		`msg="poll connection failed" connection=` + bobFull[:12] + ` err="events.list status 403: accessNotConfigured: Google Calendar API has not been used`,
		`msg="job scheduled" job=` + id + ` start=` + t1.Format("2006-01-02T15:04:05.000Z07:00") + ` kind=meet new=true`,
		`msg="poll done" jobs_scheduled=1 jobs_dropped=0 jobs_total=1 complete=false`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{bobFull, "@example.com", "Sync", "meet.google.com", "at-rt-"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q:\n%s", leak, out)
		}
	}

	// A complete poll with the meeting gone drops the job.
	f.mu.Lock()
	f.deny["at-rt-bob"] = false
	f.mu.Unlock()
	f.set("rt-alice")
	logs.Reset()
	poll(t, cfg, now)
	if want := `msg="poll done" jobs_scheduled=0 jobs_dropped=1 jobs_total=0 complete=true`; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
}
