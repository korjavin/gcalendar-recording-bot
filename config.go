package main

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is every setting the service reads. This file is the only place
// that calls os.Getenv.
type Config struct {
	ListenAddr string
	PublicURL  string
	DataDir    string
	LogLevel   slog.Level

	GoogleClientID     string
	GoogleClientSecret string
	TokenKey           []byte // 32 bytes, AES-256 key for refresh tokens at rest

	AllowedEmailDomains []string // lower-case, non-empty
	BotInviteEmail      string
	BotDisplayName      string

	JitsiBaseURL     string
	JitsiRecorderURL string
	MeetRecorderURL  string
	RecorderSecret   string

	WebhookURL    string
	WebhookSecret string

	SMTPHost     string
	SMTPPort     int
	SMTPUser     string
	SMTPPassword string
	SMTPFrom     string

	PollInterval time.Duration
	JoinLead     time.Duration
	JoinTimeout  time.Duration
	Overrun      time.Duration
	EmptyGrace   time.Duration
	MinRecording time.Duration
}

// loadConfig reads the environment. Errors name the variable, never its value.
func loadConfig() (*Config, error) {
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	str := func(name, def string) string {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
		return def
	}
	required := func(name string) string {
		v := str(name, "")
		if v == "" {
			fail("%s is required", name)
		}
		return v
	}
	integer := func(name string, def int) int {
		v := str(name, "")
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fail("%s must be a positive integer", name)
			return def
		}
		return n
	}
	seconds := func(name string, def int) time.Duration {
		return time.Duration(integer(name, def)) * time.Second
	}

	c := &Config{
		ListenAddr: str("LISTEN_ADDR", ":8080"),
		PublicURL:  strings.TrimRight(required("PUBLIC_URL"), "/"),
		DataDir:    str("DATA_DIR", "/data"),

		GoogleClientID:     required("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: required("GOOGLE_CLIENT_SECRET"),

		BotInviteEmail: strings.ToLower(required("BOT_INVITE_EMAIL")),
		BotDisplayName: str("BOT_DISPLAY_NAME", "NoteTaker"),

		JitsiBaseURL:     strings.TrimRight(str("JITSI_BASE_URL", ""), "/"),
		JitsiRecorderURL: strings.TrimRight(str("JITSI_RECORDER_URL", ""), "/"),
		MeetRecorderURL:  strings.TrimRight(str("MEET_RECORDER_URL", ""), "/"),
		RecorderSecret:   required("RECORDER_SECRET"),

		WebhookURL:    required("WEBHOOK_URL"),
		WebhookSecret: required("WEBHOOK_SECRET"),

		SMTPHost:     str("SMTP_HOST", ""), // empty: e-mail disabled
		SMTPPort:     integer("SMTP_PORT", 587),
		SMTPUser:     str("SMTP_USER", ""),
		SMTPPassword: str("SMTP_PASSWORD", ""),
		SMTPFrom:     str("SMTP_FROM", ""),

		PollInterval: seconds("POLL_INTERVAL_S", 300),
		JoinLead:     seconds("JOIN_LEAD_S", 90),
		JoinTimeout:  seconds("JOIN_TIMEOUT_S", 1200),
		Overrun:      seconds("OVERRUN_S", 1800),
		EmptyGrace:   seconds("EMPTY_GRACE_S", 60),
		MinRecording: seconds("MIN_RECORDING_S", 60),
	}

	if err := c.LogLevel.UnmarshalText([]byte(str("LOG_LEVEL", "info"))); err != nil {
		fail("LOG_LEVEL must be debug, info, warn or error")
	}

	if k := required("TOKEN_KEY"); k != "" {
		key, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(key) != 32 {
			fail("TOKEN_KEY must be base64 of exactly 32 bytes")
		}
		c.TokenKey = key
	}

	for _, d := range strings.Split(os.Getenv("ALLOWED_EMAIL_DOMAINS"), ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			c.AllowedEmailDomains = append(c.AllowedEmailDomains, d)
		}
	}
	if len(c.AllowedEmailDomains) == 0 {
		fail("ALLOWED_EMAIL_DOMAINS is required")
	}

	if c.JitsiRecorderURL == "" && c.MeetRecorderURL == "" {
		fail("JITSI_RECORDER_URL or MEET_RECORDER_URL is required")
	}
	if c.JitsiRecorderURL != "" && c.JitsiBaseURL == "" {
		fail("JITSI_BASE_URL is required with JITSI_RECORDER_URL")
	}
	if c.SMTPHost != "" && c.SMTPFrom == "" {
		fail("SMTP_FROM is required with SMTP_HOST")
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return c, nil
}
