package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// mailer sends the plain-text notifications of docs/design.md §5. Sending is
// best effort: every message goes out in the background with a few retries,
// and a failure is only logged.
type mailer struct {
	host, addr, user, password string
	from                       string // From header, as configured
	sender                     string // envelope address

	retries   []time.Duration // pause before each retry
	timeout   time.Duration   // whole SMTP conversation, per attempt
	tlsConfig *tls.Config     // STARTTLS; nil means ServerName = host
}

func newMailer(cfg *Config) *mailer {
	if cfg.SMTPHost == "" {
		slog.Info("e-mail disabled", "var", "SMTP_HOST")
		return &mailer{}
	}
	sender := cfg.SMTPFrom // "notetaker@example.com" or "NoteTaker <notetaker@example.com>"
	if a, err := mail.ParseAddress(cfg.SMTPFrom); err == nil {
		sender = a.Address
	}
	return &mailer{
		sender:   sender,
		host:     cfg.SMTPHost,
		addr:     net.JoinHostPort(cfg.SMTPHost, strconv.Itoa(cfg.SMTPPort)),
		user:     cfg.SMTPUser,
		password: cfg.SMTPPassword,
		from:     cfg.SMTPFrom,
		retries:  []time.Duration{time.Minute, 5 * time.Minute},
		timeout:  time.Minute,
	}
}

// One function per trigger. job identifies the message in logs (a job id, or
// a hashed e-mail for connection mail); it never ends up in the message.

func (m *mailer) mailLobby(job string, to []string, title string) {
	m.send(job, to, "NoteTaker is waiting in the lobby: "+title,
		"NoteTaker is waiting in the lobby of \""+title+"\".\nPlease admit it so the meeting can be recorded.\n")
}

func (m *mailer) mailTooShort(job string, to []string, title string) {
	m.send(job, to, "Recording too short: "+title,
		"The recording of \""+title+"\" was too short to transcribe and was discarded.\n")
}

func (m *mailer) mailFailed(job string, to []string, title, reason string) {
	m.send(job, to, "Recording failed: "+title+" ("+oneLine(reason)+")",
		"The recording of \""+title+"\" failed: "+oneLine(reason)+"\n")
}

func (m *mailer) mailTranscript(job string, to []string, title, content string) {
	m.send(job, to, "Transcript ready: "+title, content)
}

func (m *mailer) mailDisconnected(job, to string) {
	m.send(job, []string{to}, "Calendar disconnected",
		"NoteTaker lost access to your Google Calendar, so it will not record your meetings any more.\nConnect it again on the NoteTaker page to resume.\n")
}

// send returns at once; delivery happens in the background.
func (m *mailer) send(job string, to []string, subject, body string) {
	if m.host == "" || len(to) == 0 {
		return
	}
	msg := m.compose(to, subject, body, time.Now())
	go func() {
		for attempt := 0; ; attempt++ {
			err := m.deliver(to, msg)
			if err == nil {
				return
			}
			// SMTP reply text often quotes the address, so log only its code.
			var te *textproto.Error
			var logErr any = err
			if errors.As(err, &te) {
				logErr = te.Code
			}
			if te != nil && te.Code >= 500 || attempt >= len(m.retries) {
				slog.Warn("e-mail lost", "job", job, "attempt", attempt+1, "err", logErr)
				return
			}
			slog.Info("e-mail retry", "job", job, "attempt", attempt+1, "err", logErr)
			time.Sleep(m.retries[attempt])
		}
	}()
}

func (m *mailer) compose(to []string, subject, body string, now time.Time) []byte {
	id := make([]byte, 16)
	rand.Read(id)
	domain := m.sender[strings.LastIndexByte(m.sender, '@')+1:]
	var b strings.Builder
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), domain)
	fmt.Fprintf(&b, "From: %s\r\n", m.from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", oneLine(subject)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(body) // the DATA writer turns \n into \r\n and dot-stuffs
	return []byte(b.String())
}

// deliver is smtp.SendMail with a deadline, so a stuck server cannot pin a
// goroutine forever.
func (m *mailer) deliver(to []string, msg []byte) error {
	conn, err := net.DialTimeout("tcp", m.addr, m.timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(m.timeout))
	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		cfg := m.tlsConfig
		if cfg == nil {
			cfg = &tls.Config{ServerName: m.host}
		}
		if err := c.StartTLS(cfg); err != nil {
			return err
		}
	}
	if m.user != "" {
		// PlainAuth refuses to send the password without TLS (except to localhost).
		if err := c.Auth(smtp.PlainAuth("", m.user, m.password, m.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(m.sender); err != nil {
		return err
	}
	// A mailbox rejected for good must not cost the others their mail;
	// anything else (4xx, network) fails the attempt so it is retried whole.
	accepted := 0
	var rcptErr error
	for _, r := range to {
		err := c.Rcpt(r)
		var te *textproto.Error
		switch {
		case err == nil:
			accepted++
		case errors.As(err, &te) && te.Code >= 500:
			rcptErr = err
		default:
			return err
		}
	}
	if accepted == 0 {
		return rcptErr
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	c.Quit() // the message is queued; a lost QUIT reply must not trigger a resend
	return nil
}

// oneLine keeps calendar-supplied text from breaking a header.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
