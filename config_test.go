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

// setEnv clears every config variable, so the caller's shell can't leak in,
// then applies env.
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, k := range []string{
		"LISTEN_ADDR", "PUBLIC_URL", "DATA_DIR", "LOG_LEVEL",
		"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "TOKEN_KEY",
		"ALLOWED_EMAIL_DOMAINS", "BOT_INVITE_EMAIL", "BOT_DISPLAY_NAME",
		"JITSI_BASE_URL", "JITSI_RECORDER_URL", "MEET_RECORDER_URL", "RECORDER_SECRET",
		"WEBHOOK_URL", "WEBHOOK_SECRET",
		"SMTP_HOST", "SMTP_PORT", "SMTP_USER", "SMTP_PASSWORD", "SMTP_FROM",
		"POLL_INTERVAL_S", "JOIN_LEAD_S", "JOIN_TIMEOUT_S", "OVERRUN_S", "EMPTY_GRACE_S", "MIN_RECORDING_S",
	} {
		t.Setenv(k, "")
	}
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
		t.Errorf("defaults: %q %q %q", c.PublicURL, c.ListenAddr, c.DataDir)
	}
	if strings.Join(c.AllowedEmailDomains, ",") != "example.com,example.org" {
		t.Errorf("domains = %v", c.AllowedEmailDomains)
	}
	if c.BotInviteEmail != "notetaker@example.com" || len(c.TokenKey) != 32 {
		t.Errorf("invite/key: %q %d", c.BotInviteEmail, len(c.TokenKey))
	}
	if c.PollInterval != 300*time.Second || c.JoinLead != 90*time.Second || c.SMTPPort != 587 {
		t.Errorf("durations: %v %v %d", c.PollInterval, c.JoinLead, c.SMTPPort)
	}

	t.Setenv("SMTP_HOST", "") // e-mail disabled: SMTP_FROM not needed either
	t.Setenv("SMTP_FROM", "")
	if _, err := loadConfig(); err != nil {
		t.Errorf("without SMTP_HOST: %v", err)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	for _, name := range []string{
		"PUBLIC_URL", "GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "TOKEN_KEY",
		"ALLOWED_EMAIL_DOMAINS", "BOT_INVITE_EMAIL", "RECORDER_SECRET",
		"WEBHOOK_URL", "WEBHOOK_SECRET", "SMTP_FROM", "MEET_RECORDER_URL",
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
