package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func validEnv() map[string]string {
	return map[string]string{
		"PUBLIC_URL":            "https://bot.example.com/",
		"GOOGLE_CLIENT_ID":      "client-id",
		"GOOGLE_CLIENT_SECRET":  "client-secret-value",
		"TOKEN_KEY":             base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"ALLOWED_EMAIL_DOMAINS": " Example.com, ,example.org ",
		"BOT_INVITE_EMAIL":      "NoteTaker@example.com",
		"MEET_RECORDER_URL":     "http://meet-recorder:8080",
		"RECORDER_SECRET":       "recorder-secret-value",
		"WEBHOOK_URL":           "http://transcriber:8080/webhook",
		"WEBHOOK_SECRET":        "webhook-secret-value",
		"SMTP_HOST":             "smtp.example.com",
		"SMTP_FROM":             "notetaker@example.com",
	}
}

func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestLoadConfigValid(t *testing.T) {
	setEnv(t, validEnv())
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://bot.example.com" || c.ListenAddr != ":8080" || c.DataDir != "/data" {
		t.Errorf("defaults: %+v", c)
	}
	if strings.Join(c.AllowedEmailDomains, ",") != "example.com,example.org" {
		t.Errorf("domains = %v", c.AllowedEmailDomains)
	}
	if c.BotInviteEmail != "notetaker@example.com" || len(c.TokenKey) != 32 {
		t.Errorf("invite/key: %q %d", c.BotInviteEmail, len(c.TokenKey))
	}
	if c.PollInterval != 300*time.Second || c.JoinLead != 90*time.Second || c.SMTPPort != 587 {
		t.Errorf("durations: %+v", c)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	for _, name := range []string{
		"PUBLIC_URL", "GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "TOKEN_KEY",
		"ALLOWED_EMAIL_DOMAINS", "BOT_INVITE_EMAIL", "RECORDER_SECRET",
		"WEBHOOK_URL", "WEBHOOK_SECRET", "SMTP_HOST", "SMTP_FROM", "MEET_RECORDER_URL",
	} {
		t.Run(name, func(t *testing.T) {
			setEnv(t, validEnv())
			t.Setenv(name, "")
			_, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("err = %v, want it to name %s", err, name)
			}
		})
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("sixteen-byte-key"))
	cases := []struct{ name, value string }{
		{"TOKEN_KEY", short},
		{"TOKEN_KEY", "not base64 !!"},
		{"ALLOWED_EMAIL_DOMAINS", " , "},
		{"POLL_INTERVAL_S", "soon"},
		{"SMTP_PORT", "-1"},
		{"LOG_LEVEL", "loud"},
		{"JITSI_RECORDER_URL", "http://jitsi-recorder:8080"}, // without JITSI_BASE_URL
	}
	for _, tc := range cases {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			setEnv(t, validEnv())
			t.Setenv(tc.name, tc.value)
			_, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("err = %v, want it to name %s", err, tc.name)
			}
			if strings.TrimSpace(tc.value) != "" && strings.Contains(err.Error(), tc.value) {
				t.Fatalf("err leaks the value: %v", err)
			}
		})
	}
}

func TestHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux().ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}
