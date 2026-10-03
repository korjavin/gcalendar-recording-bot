package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type trReq struct {
	event, sig, body string
}

// fakeTranscriber answers with the queued statuses, then 200.
type fakeTranscriber struct {
	mu       sync.Mutex
	statuses []int
	reqs     []trReq
}

func newFakeTranscriber(t *testing.T, cfg *Config, statuses ...int) *fakeTranscriber {
	f := &fakeTranscriber{statuses: statuses}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reqs = append(f.reqs, trReq{r.Header.Get("x-jitsi-capture-event"), r.Header.Get("x-jitsi-capture-signature"), string(raw)})
		status := http.StatusOK
		if len(f.statuses) > 0 {
			status, f.statuses = f.statuses[0], f.statuses[1:]
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	cfg.WebhookURL, cfg.WebhookSecret = srv.URL+"/webhook", "hook-secret"
	old := webhookRetries
	webhookRetries = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { webhookRetries = old })
	return f
}

func (f *fakeTranscriber) requests() []trReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func waitHandedOff(t *testing.T, cfg *Config, id string) {
	t.Helper()
	for range 200 {
		jobsMu.Lock()
		j, err := readJob(cfg.DataDir, id)
		jobsMu.Unlock()
		if err == nil && j.HandedOff {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job not handed off")
}

func TestHandOffGoldenBodies(t *testing.T) {
	const id = "cal-0000000000000001"
	tests := []struct{ name, url, event, want string }{
		{"jitsi with tracks", jitsiLink,
			`{"event":"recording.finished","id":"` + id + `","started_at":"2026-01-01T10:00:00Z","ended_at":"2026-01-01T10:30:34Z","duration_s":1834.2,"reason":"empty_room","participants":["Alice","Bob"],"artifacts":[` +
				`{"kind":"audio","path":"/data/jitsi/` + id + `/audio.webm","format":"webm"},` +
				`{"kind":"track","path":"/data/jitsi/` + id + `/tracks/p1.webm","participant_id":"p1","name":"Alice","offset_s":0,"ended_s":1834.2},` +
				`{"kind":"track","path":"/data/jitsi/` + id + `/tracks/p2.webm","participant_id":"p2","name":"Bob","offset_s":12.5,"ended_s":900},` +
				`{"kind":"speakers","path":"/data/jitsi/` + id + `/tracks/speakers.jsonl"},{"kind":"future","path":"/x"}]}`,
			`{"event":"recording.finished","id":"` + id + `","title":"Weekly","jitsi_url":"` + jitsiLink + `","audio_path":"/data/jitsi/` + id + `/audio.webm",` +
				`"duration_s":1834.2,"started_at":"2026-01-01T10:00:00Z","ended_at":"2026-01-01T10:30:34Z","participants":["Alice","Bob"],"callback_url":"https://bot.example.com/notify",` +
				`"tracks":[{"id":"p1","name":"Alice","path":"/data/jitsi/` + id + `/tracks/p1.webm","offset_s":0,"ended_s":1834.2},` +
				`{"id":"p2","name":"Bob","path":"/data/jitsi/` + id + `/tracks/p2.webm","offset_s":12.5,"ended_s":900}]}`},
		{"meet with captions", meetLink,
			`{"event":"recording.finished","id":"` + id + `","started_at":"2026-01-01T10:00:00Z","ended_at":"2026-01-01T10:05:00Z","duration_s":300,"reason":"ended","participants":["Alice"],"artifacts":[` +
				`{"kind":"audio","path":"/data/meet/` + id + `/audio.wav","format":"wav"},{"kind":"captions","path":"/data/meet/` + id + `/captions.jsonl"}]}`,
			`{"event":"recording.finished","id":"` + id + `","title":"Weekly","jitsi_url":"` + meetLink + `","source":"meet","audio_path":"/data/meet/` + id + `/audio.wav",` +
				`"duration_s":300,"started_at":"2026-01-01T10:00:00Z","ended_at":"2026-01-01T10:05:00Z","participants":["Alice"],"callback_url":"https://bot.example.com/notify",` +
				`"speaker_hints_path":"/data/meet/` + id + `/captions.jsonl"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _, _ := schedulerSetup(t)
			tr := newFakeTranscriber(t, cfg)
			putJob(t, cfg, id, stateStarted, tc.url, time.Now())
			if code := postEvent(cfg, tc.event, ""); code != 200 {
				t.Fatalf("status %d", code)
			}
			waitHandedOff(t, cfg, id)
			r := tr.requests()
			if len(r) != 1 {
				t.Fatalf("%d requests, want 1", len(r))
			}
			if r[0].body != tc.want {
				t.Errorf("body\n got %s\nwant %s", r[0].body, tc.want)
			}
			if r[0].event != "recording.finished" || r[0].sig != sign("hook-secret", []byte(r[0].body)) {
				t.Errorf("headers: event %q, sig %q", r[0].event, r[0].sig)
			}
		})
	}
}

func TestHandOffRetriesAfter500(t *testing.T) {
	cfg, _, _, _ := schedulerSetup(t)
	tr := newFakeTranscriber(t, cfg, 500, 302)
	const id = "cal-0000000000000001"
	putJob(t, cfg, id, stateStarted, meetLink, time.Now())
	postEvent(cfg, `{"event":"recording.finished","id":"`+id+`","duration_s":120}`, "")
	waitHandedOff(t, cfg, id)
	if n := len(tr.requests()); n != 3 {
		t.Errorf("%d requests, want 3 (500, redirect not followed, 200)", n)
	}
}

// A finished job whose hand-off never got through (retries exhausted, or a
// crash right after the state write) is sent again by the sweep.
func TestHandOffSweep(t *testing.T) {
	cfg, _, _, _ := schedulerSetup(t)
	tr := newFakeTranscriber(t, cfg, 500, 500, 500)
	const id = "cal-0000000000000001"
	putJob(t, cfg, id, stateStarted, meetLink, time.Now())
	postEvent(cfg, `{"event":"recording.finished","id":"`+id+`","duration_s":120}`, "")
	for len(tr.requests()) < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	for { // wait for the delivery goroutine to give up
		if _, busy := handOffs.Load(id); !busy {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := pendingHandOffs(cfg); !slices.Equal(got, []string{id}) {
		t.Fatalf("pending %v, want [%s]", got, id)
	}
	for _, id := range pendingHandOffs(cfg) {
		handOff(cfg, id)
	}
	waitHandedOff(t, cfg, id)
	if got := pendingHandOffs(cfg); len(got) != 0 {
		t.Errorf("pending after sweep %v", got)
	}
	handOff(cfg, id) // already handed off: nothing sent
	if n := len(tr.requests()); n != 4 {
		t.Errorf("%d requests, want 4", n)
	}
}

func postNotify(cfg *Config, body, sig string) int {
	if sig == "" {
		sig = sign(cfg.WebhookSecret, []byte(body))
	}
	req := httptest.NewRequest("POST", "/notify", strings.NewReader(body))
	req.Header.Set("x-jitsi-capture-signature", sig)
	rec := httptest.NewRecorder()
	webMux(cfg).ServeHTTP(rec, req)
	return rec.Code
}

func TestNotify(t *testing.T) {
	cfg, _, _, smtp := schedulerSetup(t)
	newFakeTranscriber(t, cfg)
	const id = "cal-0000000000000001"
	putJob(t, cfg, id, stateStarted, meetLink, time.Now())
	postEvent(cfg, `{"event":"recording.finished","id":"`+id+`","duration_s":120}`, "")
	waitHandedOff(t, cfg, id)
	putJob(t, cfg, "cal-0000000000000002", stateStarted, meetLink, time.Now()) // never handed off

	ok := `{"id":"` + id + `","content":"Transcript ready: https://outline.example.com/doc/x"}`
	for name, tc := range map[string]struct {
		body, sig string
		code      int
	}{
		"bad signature":   {ok, "sha256=00", 401},
		"wrong secret":    {ok, sign("other", []byte(ok)), 401},
		"missing content": {`{"id":"` + id + `"}`, "", 400},
		"not json":        {`x`, "", 400},
		"unknown job":     {`{"id":"cal-00000000000000ff","content":"x"}`, "", 404},
		"not handed off":  {`{"id":"cal-0000000000000002","content":"x"}`, "", 404},
		"path id":         {`{"id":"../connections/x","content":"x"}`, "", 404},
	} {
		if code := postNotify(cfg, tc.body, tc.sig); code != tc.code {
			t.Errorf("%s: %d, want %d", name, code, tc.code)
		}
	}
	smtp.none(t)

	if code := postNotify(cfg, ok, ""); code != 200 {
		t.Fatalf("happy path: %d", code)
	}
	smtp.subject(t, "Transcript ready: Weekly")
}
