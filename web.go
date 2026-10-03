package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Google endpoints; tests point them at httptest servers.
var (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
)

const (
	calendarScope   = "https://www.googleapis.com/auth/calendar.readonly"
	oauthScopes     = "openid email " + calendarScope
	stateCookieName = "oauth_state"
)

// errNoCalendar: the user unticked calendar access on Google's consent screen.
var errNoCalendar = errors.New("calendar scope not granted")

var httpClient = &http.Client{Timeout: 30 * time.Second}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}}</title></head>
<body style="font-family: sans-serif; max-width: 40em; margin: 2em auto; padding: 0 1em">
<h1>{{.Name}}</h1>
{{if .Message}}<p>{{.Message}}</p>{{end}}
{{if .Home}}
<p>{{.Name}} records the online meetings you invite it to, has them transcribed,
and e-mails you when the transcript is ready.</p>
<ol>
<li>Connect your Google Calendar (read-only access).</li>
<li>Invite <b>{{.Invite}}</b> to any meeting with a Google Meet or Jitsi link that you want recorded.</li>
</ol>
<p><a href="/connect">Connect Google Calendar</a></p>
{{else}}
<p>Invite <b>{{.Invite}}</b> to meetings you want recorded.</p>
<p><a href="/">Back</a></p>
{{end}}
</body></html>
`))

type page struct {
	Name, Invite, Message string
	Home                  bool
}

func render(w http.ResponseWriter, cfg *Config, status int, p page) {
	p.Name, p.Invite = cfg.BotDisplayName, cfg.BotInviteEmail
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pageTmpl.Execute(w, p); err != nil {
		slog.Error("render page", "err", err)
	}
}

// webMux adds the public connect pages to the base mux.
func webMux(cfg *Config) *http.ServeMux {
	mux := newMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, cfg, http.StatusOK, page{Home: true})
	})
	mux.HandleFunc("GET /connect", func(w http.ResponseWriter, r *http.Request) { handleConnect(w, r, cfg) })
	mux.HandleFunc("GET /oauth/callback", func(w http.ResponseWriter, r *http.Request) { handleCallback(w, r, cfg) })
	mux.HandleFunc("POST /events", func(w http.ResponseWriter, r *http.Request) { handleEvents(w, r, cfg) })
	mux.HandleFunc("POST /notify", func(w http.ResponseWriter, r *http.Request) { handleNotify(w, r, cfg) })
	return mux
}

func redirectURI(cfg *Config) string { return cfg.PublicURL + "/oauth/callback" }

func handleConnect(w http.ResponseWriter, r *http.Request, cfg *Config) {
	state := rand.Text()
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: state, Path: "/oauth/callback", MaxAge: 600,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	q := url.Values{
		"client_id":     {cfg.GoogleClientID},
		"redirect_uri":  {redirectURI(cfg)},
		"response_type": {"code"},
		"scope":         {oauthScopes},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	http.Redirect(w, r, googleAuthURL+"?"+q.Encode(), http.StatusFound)
}

func handleCallback(w http.ResponseWriter, r *http.Request, cfg *Config) {
	// The state cookie is single-use.
	http.SetCookie(w, &http.Cookie{Name: stateCookieName, Path: "/oauth/callback", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	c, err := r.Cookie(stateCookieName)
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		render(w, cfg, http.StatusBadRequest, page{Message: "This sign-in link is invalid or expired. Please start again."})
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		render(w, cfg, http.StatusBadRequest, page{Message: "Google did not grant access, so nothing was connected."})
		return
	}

	refresh, email, err := exchangeCode(r, cfg, code)
	if errors.Is(err, errNoCalendar) {
		render(w, cfg, http.StatusBadRequest, page{Message: "Calendar access was not granted, so nothing was connected. Please connect again and keep the calendar permission ticked on Google's consent screen."})
		return
	}
	if err != nil {
		slog.Warn("oauth exchange failed", "err", err)
		render(w, cfg, http.StatusBadGateway, page{Message: "Connecting to Google failed. Please try again."})
		return
	}
	if !emailAllowed(email, cfg.AllowedEmailDomains) {
		slog.Info("connect refused: domain not allowed", "email_hash", emailHash(email)[:12])
		render(w, cfg, http.StatusForbidden, page{Message: "Accounts from this domain cannot connect to this bot."})
		return
	}
	if err := saveConnection(cfg.DataDir, cfg.TokenKey, email, refresh, time.Now()); err != nil {
		slog.Error("store connection", "email_hash", emailHash(email)[:12], "err", err)
		render(w, cfg, http.StatusInternalServerError, page{Message: "Saving the connection failed. Please try again."})
		return
	}
	slog.Info("calendar connected", "email_hash", emailHash(email)[:12])
	render(w, cfg, http.StatusOK, page{Message: "Connected as " + email + "."})
}

// exchangeCode trades the authorization code for tokens and returns the
// refresh token and the verified, lower-cased e-mail from the ID token.
func exchangeCode(r *http.Request, cfg *Config, code string) (refresh, email string, err error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {cfg.GoogleClientID},
		"client_secret": {cfg.GoogleClientSecret},
		"redirect_uri":  {redirectURI(cfg)},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("token endpoint status %d", resp.StatusCode) // body may echo secrets; not logged
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", "", errors.New("token response is not JSON")
	}
	if !slices.Contains(strings.Fields(tok.Scope), calendarScope) {
		return "", "", errNoCalendar
	}
	if tok.RefreshToken == "" {
		return "", "", errors.New("token response has no refresh_token")
	}
	email, err = idTokenEmail(tok.IDToken, cfg.GoogleClientID)
	if err != nil {
		return "", "", err
	}
	return tok.RefreshToken, email, nil
}

// idTokenEmail reads the e-mail claim. The ID token came straight from
// Google's token endpoint over TLS, so its signature is not re-checked
// (as Google's OpenID Connect guide allows); audience and verification are.
func idTokenEmail(idToken, clientID string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("malformed id_token payload")
	}
	var claims struct {
		Aud           string `json:"aud"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("malformed id_token claims")
	}
	if claims.Aud != clientID {
		return "", errors.New("id_token audience mismatch")
	}
	if claims.Email == "" || !claims.EmailVerified {
		return "", errors.New("id_token has no verified email")
	}
	return strings.ToLower(claims.Email), nil
}

func emailAllowed(email string, domains []string) bool {
	at := strings.LastIndexByte(email, '@')
	return at > 0 && slices.Contains(domains, strings.ToLower(email[at+1:]))
}
