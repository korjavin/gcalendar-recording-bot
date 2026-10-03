package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Calendar API base; tests point it at an httptest server.
var googleCalendarURL = "https://www.googleapis.com/calendar/v3"

// errInvalidGrant: the user revoked the bot's access (or the token expired).
var errInvalidGrant = errors.New("invalid_grant")

// jobsMu serialises every read-modify-write under DATA_DIR/jobs. The planner
// holds it for a whole reconcile; whoever moves a job out of "scheduled" must
// hold it too, so the planner never overwrites a job that has just started.
var jobsMu sync.Mutex

// notifier sends every e-mail; main replaces it with the configured mailer.
var notifier = &mailer{}

// job is DATA_DIR/jobs/<id>/job.json. The planner writes only state
// "scheduled"; later states belong to the scheduler.
type job struct {
	ID     string    `json:"id"`
	State  string    `json:"state"`
	Title  string    `json:"title"`
	URL    string    `json:"url"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Notify []string  `json:"notify"`
	Events []string  `json:"events,omitempty"` // recorder events handled, for idempotency
	Error  string    `json:"error,omitempty"`
}

const stateScheduled = "scheduled"

// calEvent is the part of a Calendar API event the planner reads.
type calEvent struct {
	ICalUID        string `json:"iCalUID"`
	Status         string `json:"status"`
	Summary        string `json:"summary"`
	Location       string `json:"location"`
	Description    string `json:"description"`
	HangoutLink    string `json:"hangoutLink"`
	ConferenceData struct {
		EntryPoints []entryPoint `json:"entryPoints"`
	} `json:"conferenceData"`
	Attendees []attendee `json:"attendees"`
	Start     struct {
		DateTime time.Time `json:"dateTime"`
	} `json:"start"`
	End struct {
		DateTime time.Time `json:"dateTime"`
	} `json:"end"`
}

type entryPoint struct {
	EntryPointType string `json:"entryPointType"`
	URI            string `json:"uri"`
}

type attendee struct {
	Email          string `json:"email"`
	ResponseStatus string `json:"responseStatus"`
}

// jobID is the same for every attendee's copy of one occurrence.
func jobID(iCalUID string, start time.Time) string {
	sum := sha256.Sum256([]byte(iCalUID + "|" + start.UTC().Format(time.RFC3339)))
	return "cal-" + hex.EncodeToString(sum[:])[:16]
}

// meetURL normalises a Google Meet link to https://meet.google.com/<code>,
// or returns "" when it is not one.
func meetURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "meet.google.com") {
		return ""
	}
	code := strings.Trim(u.Path, "/")
	if code == "" || strings.ContainsAny(code, "/") {
		return ""
	}
	return "https://meet.google.com/" + code
}

var urlRe = regexp.MustCompile(`https?://[^\s"'<>]+`)

// jitsiURL returns the first link to a room under base found in text, or "".
func jitsiURL(text, base string) string {
	if base == "" {
		return ""
	}
	prefix := strings.ToLower(base) + "/"
	for _, u := range urlRe.FindAllString(html.UnescapeString(text), -1) {
		u = strings.TrimRight(u, ".,;:!?)]}")
		if strings.HasPrefix(strings.ToLower(u), prefix) && len(u) > len(prefix) {
			return u
		}
	}
	return ""
}

// callURL picks the event's call link: Meet first, then Jitsi.
func callURL(ev *calEvent, jitsiBase string) string {
	for _, ep := range ev.ConferenceData.EntryPoints {
		if ep.EntryPointType == "video" {
			if u := meetURL(ep.URI); u != "" {
				return u
			}
		}
	}
	if u := meetURL(ev.HangoutLink); u != "" {
		return u
	}
	if u := jitsiURL(ev.Location, jitsiBase); u != "" {
		return u
	}
	return jitsiURL(ev.Description, jitsiBase)
}

// candidateURL applies the recording rule (design §2) and returns the call
// link, or "" when the event is not recorded.
func candidateURL(ev *calEvent, botEmail, jitsiBase string) string {
	if ev.Status == "cancelled" || ev.ICalUID == "" || ev.Start.DateTime.IsZero() {
		return "" // all-day events have no dateTime and no call time
	}
	invited := slices.ContainsFunc(ev.Attendees, func(a attendee) bool {
		return strings.EqualFold(a.Email, botEmail) && a.ResponseStatus != "declined"
	})
	if !invited {
		return ""
	}
	return callURL(ev, jitsiBase)
}

// runPlanner polls every PollInterval until ctx ends, calling afterFirst
// once the first poll is over (so stale jobs from before a restart are
// reconciled before anything starts them).
func runPlanner(ctx context.Context, cfg *Config, afterFirst func()) {
	t := time.NewTicker(cfg.PollInterval)
	defer t.Stop()
	for {
		if err := pollOnce(ctx, cfg, time.Now()); err != nil {
			slog.Error("poll failed", "err", err)
		}
		if afterFirst != nil {
			afterFirst()
			afterFirst = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pollOnce lists every connection's next 24 hours and reconciles the
// scheduled jobs with the candidates found.
func pollOnce(ctx context.Context, cfg *Config, now time.Time) error {
	ents, err := os.ReadDir(filepath.Join(cfg.DataDir, "connections"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	cands := map[string]*job{}
	complete := true // every connection was listed; only then may jobs be dropped
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(cfg.DataDir, "connections", e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) { // not just removed since ReadDir
				complete = false
				slog.Warn("read connection failed", "connection", strings.TrimSuffix(e.Name(), ".json"), "err", err)
			}
			continue
		}
		email, err := pollConnection(ctx, cfg, data, now, cands)
		switch {
		case errors.Is(err, errInvalidGrant):
			if dropConnection(path, data) {
				slog.Info("calendar access revoked; connection dropped", "email_hash", emailHash(email)[:12])
				notifier.mailDisconnected(emailHash(email)[:12], email)
			} else {
				complete = false // reconnected meanwhile; its calendar is unpolled
			}
		case err != nil:
			complete = false
			slog.Warn("poll connection failed", "connection", strings.TrimSuffix(e.Name(), ".json"), "err", err)
		}
	}
	return reconcile(cfg.DataDir, cands, complete)
}

// pollConnection adds the connection's candidates to cands and returns the
// connection's e-mail (for logs and the disconnect notice).
func pollConnection(ctx context.Context, cfg *Config, data []byte, now time.Time, cands map[string]*job) (string, error) {
	var c connection
	if err := json.Unmarshal(data, &c); err != nil {
		return "", fmt.Errorf("connection file: %w", err)
	}
	refresh, err := decryptToken(cfg.TokenKey, c.RefreshToken, c.Email)
	if err != nil {
		return c.Email, fmt.Errorf("decrypt token: %w", err)
	}
	access, err := refreshAccessToken(ctx, cfg, refresh)
	if err != nil {
		return c.Email, err
	}
	events, err := listEvents(ctx, access, now)
	if err != nil {
		return c.Email, err
	}
	for i := range events {
		ev := &events[i]
		u := candidateURL(ev, cfg.BotInviteEmail, cfg.JitsiBaseURL)
		if u == "" {
			continue
		}
		id := jobID(ev.ICalUID, ev.Start.DateTime)
		j := cands[id]
		if j == nil {
			j = &job{ID: id, State: stateScheduled, Title: ev.Summary, URL: u,
				Start: ev.Start.DateTime.UTC(), End: ev.End.DateTime.UTC()}
			cands[id] = j
		}
		if !slices.Contains(j.Notify, c.Email) {
			j.Notify = append(j.Notify, c.Email)
		}
	}
	return c.Email, nil
}

// refreshAccessToken trades a refresh token for an access token.
func refreshAccessToken(ctx context.Context, cfg *Config, refresh string) (string, error) {
	form := url.Values{
		"client_id":     {cfg.GoogleClientID},
		"client_secret": {cfg.GoogleClientSecret},
		"refresh_token": {refresh},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("token endpoint status %d: response is not JSON", resp.StatusCode)
	}
	if tok.Error == "invalid_grant" {
		return "", errInvalidGrant
	}
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		return "", fmt.Errorf("token endpoint status %d", resp.StatusCode)
	}
	return tok.AccessToken, nil
}

// listEvents returns the primary calendar's single events in [now, now+24h),
// following every page.
func listEvents(ctx context.Context, access string, now time.Time) ([]calEvent, error) {
	var all []calEvent
	pageToken := ""
	for {
		q := url.Values{
			"singleEvents": {"true"},
			"orderBy":      {"startTime"},
			"timeMin":      {now.UTC().Format(time.RFC3339)},
			"timeMax":      {now.Add(24 * time.Hour).UTC().Format(time.RFC3339)},
		}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleCalendarURL+"/calendars/primary/events?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+access)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items         []calEvent `json:"items"`
			NextPageToken string     `json:"nextPageToken"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&page)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("events.list status %d", resp.StatusCode)
		}
		if err != nil {
			return nil, fmt.Errorf("events.list response: %w", err)
		}
		all = append(all, page.Items...)
		if page.NextPageToken == "" {
			return all, nil
		}
		pageToken = page.NextPageToken
	}
}

func jobPath(dataDir, id string) string { return filepath.Join(dataDir, "jobs", id, "job.json") }

// reconcile writes every candidate as a scheduled job and, when the poll was
// complete, drops scheduled jobs that are no longer candidates. Jobs past
// "scheduled" are never touched. After a partial poll the notify list keeps
// existing addresses, since the failed connection's view is unknown.
func reconcile(dataDir string, cands map[string]*job, complete bool) error {
	jobsMu.Lock()
	defer jobsMu.Unlock()

	existing := map[string]*job{}
	ents, err := os.ReadDir(filepath.Join(dataDir, "jobs"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range ents {
		j, err := readJob(dataDir, e.Name())
		if err != nil {
			if errors.Is(err, errBadJob) {
				slog.Warn("unreadable job file", "job", e.Name())
			}
			continue // or not a job directory
		}
		existing[j.ID] = j
	}

	var errs []error
	for id, c := range cands {
		old := existing[id]
		if old != nil && old.State != stateScheduled {
			continue
		}
		if old != nil && !complete {
			for _, e := range old.Notify {
				if !slices.Contains(c.Notify, e) {
					c.Notify = append(c.Notify, e)
				}
			}
		}
		slices.Sort(c.Notify)
		data, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		if prev, err := os.ReadFile(jobPath(dataDir, id)); err == nil && string(prev) == string(data) {
			continue
		}
		if err := writeFileAtomic(jobPath(dataDir, id), data); err != nil {
			errs = append(errs, err)
			continue
		}
		if old == nil {
			slog.Info("job scheduled", "job", id, "start", c.Start)
		}
	}
	if complete {
		for id, j := range existing {
			if j.State == stateScheduled && cands[id] == nil {
				if err := os.RemoveAll(filepath.Join(dataDir, "jobs", id)); err != nil {
					errs = append(errs, err)
					continue
				}
				slog.Info("job dropped", "job", id)
			}
		}
	}
	return errors.Join(errs...)
}
