package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// fakeGoogle serves the token endpoint and answers every code with the
// given e-mail and refresh token.
func fakeGoogle(t *testing.T, email, refresh *string) {
	t.Helper()
	fakeGoogleScope(t, email, refresh, oauthScopes)
}

func fakeGoogleScope(t *testing.T, email, refresh *string, scope string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "good-code" ||
			r.Form.Get("client_secret") != "secret" || r.Form.Get("redirect_uri") != "https://bot.example.com/oauth/callback" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		claims, _ := json.Marshal(map[string]any{"aud": "client-id", "email": *email, "email_verified": true})
		idToken := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
		json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "refresh_token": *refresh, "id_token": idToken, "scope": scope})
	}))
	t.Cleanup(srv.Close)
	old := googleTokenURL
	googleTokenURL = srv.URL
	t.Cleanup(func() { googleTokenURL = old })
}

func testConfig(t *testing.T) *Config {
	return &Config{
		PublicURL:           "https://bot.example.com",
		DataDir:             t.TempDir(),
		GoogleClientID:      "client-id",
		GoogleClientSecret:  "secret",
		TokenKey:            bytes.Repeat([]byte{7}, 32),
		AllowedEmailDomains: []string{"example.com"},
		BotDisplayName:      "NoteTaker",
	}
}

func callback(cfg *Config, cookieState, queryState, code string) *httptest.ResponseRecorder {
	q := url.Values{"state": {queryState}, "code": {code}}
	req := httptest.NewRequest("GET", "/oauth/callback?"+q.Encode(), nil)
	if cookieState != "" {
		req.AddCookie(&http.Cookie{Name: stateCookieName, Value: cookieState})
	}
	rec := httptest.NewRecorder()
	webMux(cfg).ServeHTTP(rec, req)
	return rec
}

func readConnection(t *testing.T, cfg *Config, email string) (connection, string) {
	t.Helper()
	path := connectionPath(cfg.DataDir, email)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("connection not stored: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	var c connection
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("rt-")) {
		t.Fatal("refresh token stored in plain text")
	}
	plain, err := decryptToken(cfg.TokenKey, c.RefreshToken, c.Email)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return c, plain
}

func TestConnectRedirect(t *testing.T) {
	cfg := testConfig(t)
	rec := httptest.NewRecorder()
	webMux(cfg).ServeHTTP(rec, httptest.NewRequest("GET", "/connect", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != stateCookieName || !cookies[0].HttpOnly || !cookies[0].Secure ||
		cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Value != q.Get("state") || q.Get("state") == "" {
		t.Fatalf("bad state cookie %+v vs state %q", cookies, q.Get("state"))
	}
	if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("scope") != oauthScopes ||
		q.Get("redirect_uri") != "https://bot.example.com/oauth/callback" || q.Get("client_id") != "client-id" {
		t.Fatalf("bad auth URL query: %v", q)
	}
}

func TestCallback(t *testing.T) {
	email, refresh := "Alice@Example.com", "rt-1"
	fakeGoogle(t, &email, &refresh)
	cfg := testConfig(t)

	rec := callback(cfg, "s1", "s1", "good-code")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Connected as alice@example.com") ||
		!strings.Contains(rec.Body.String(), "#note") {
		t.Fatalf("happy path: %d %s", rec.Code, rec.Body)
	}
	c, plain := readConnection(t, cfg, "alice@example.com")
	if plain != "rt-1" || c.Email != "alice@example.com" || c.ConnectedAt.IsZero() {
		t.Fatalf("stored %+v / %q", c, plain)
	}

	// Reconnect replaces the token.
	refresh = "rt-2"
	if rec := callback(cfg, "s2", "s2", "good-code"); rec.Code != http.StatusOK {
		t.Fatalf("reconnect: %d", rec.Code)
	}
	if _, plain := readConnection(t, cfg, "alice@example.com"); plain != "rt-2" {
		t.Fatalf("reconnect stored %q", plain)
	}
	if ents, _ := os.ReadDir(cfg.DataDir + "/connections"); len(ents) != 1 {
		t.Fatalf("connections dir has %d entries, want 1", len(ents))
	}
}

func TestCallbackBadState(t *testing.T) {
	email, refresh := "alice@example.com", "rt-1"
	fakeGoogle(t, &email, &refresh)
	cfg := testConfig(t)
	for _, c := range [][2]string{{"s1", "other"}, {"", "s1"}, {"s1", ""}} {
		if rec := callback(cfg, c[0], c[1], "good-code"); rec.Code != http.StatusBadRequest {
			t.Errorf("cookie %q state %q: status %d, want 400", c[0], c[1], rec.Code)
		}
	}
	if _, err := os.Stat(cfg.DataDir + "/connections"); !os.IsNotExist(err) {
		t.Fatal("something stored on bad state")
	}
}

func TestCallbackForeignDomain(t *testing.T) {
	email, refresh := "mallory@example.org", "rt-1"
	fakeGoogle(t, &email, &refresh)
	cfg := testConfig(t)
	if rec := callback(cfg, "s1", "s1", "good-code"); rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if _, err := os.Stat(cfg.DataDir + "/connections"); !os.IsNotExist(err) {
		t.Fatal("something stored for a foreign domain")
	}
}

func TestCallbackExchangeFails(t *testing.T) {
	email, refresh := "alice@example.com", "rt-1"
	fakeGoogle(t, &email, &refresh)
	cfg := testConfig(t)
	if rec := callback(cfg, "s1", "s1", "bad-code"); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
}

func TestCallbackNoCalendarScope(t *testing.T) {
	email, refresh := "alice@example.com", "rt-1"
	fakeGoogleScope(t, &email, &refresh, "openid https://www.googleapis.com/auth/userinfo.email")
	cfg := testConfig(t)
	if rec := callback(cfg, "s1", "s1", "good-code"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "keep the calendar permission ticked") {
		t.Errorf("page does not explain the missing permission:\n%s", rec.Body.String())
	}
	if _, err := os.Stat(cfg.DataDir + "/connections"); !os.IsNotExist(err) {
		t.Fatal("connection stored without calendar scope")
	}
}

func TestHomePage(t *testing.T) {
	rec := httptest.NewRecorder()
	webMux(testConfig(t)).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `href="/connect"`) ||
		!strings.Contains(rec.Body.String(), "#note") {
		t.Fatalf("home: %d %s", rec.Code, rec.Body)
	}
}

func TestTokenSealedToOwner(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	a, _ := encryptToken(key, "tok", "a@example.com")
	b, _ := encryptToken(key, "tok", "a@example.com")
	if bytes.Equal(a, b) {
		t.Fatal("nonce reused")
	}
	if _, err := decryptToken(key, a, "b@example.com"); err == nil {
		t.Fatal("token decrypted under another owner")
	}
}
