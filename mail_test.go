package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSMTP speaks just enough SMTP: EHLO, STARTTLS, AUTH PLAIN, MAIL, RCPT,
// DATA, QUIT. The first failMail MAIL commands get a 451; RCPT bad@… a 550.
type fakeSMTP struct {
	ln       net.Listener
	tls      *tls.Config
	roots    *x509.CertPool
	failMail int32
	got      chan fakeMsg
}

type fakeMsg struct {
	auth, from string
	to         []string
	tls        bool
	data       string
}

func newFakeSMTP(t *testing.T, failMail int32) *fakeSMTP {
	ts := httptest.NewUnstartedServer(nil) // only for its 127.0.0.1 certificate
	ts.StartTLS()
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	cert := ts.TLS.Certificates[0]
	ts.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, tls: &tls.Config{Certificates: []tls.Certificate{cert}},
		roots: roots, failMail: failMail, got: make(chan fakeMsg, 10)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	var m fakeMsg
	r, w := bufio.NewReader(c), c
	say := func(s string) { io.WriteString(w, s+"\r\n") }
	say("220 fake")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch {
		case cmd == "EHLO":
			if m.tls {
				say("250-fake\r\n250 AUTH PLAIN")
			} else {
				say("250-fake\r\n250 STARTTLS")
			}
		case cmd == "STARTTLS":
			say("220 go")
			tc := tls.Server(c, f.tls)
			if tc.Handshake() != nil {
				return
			}
			c, r, w, m.tls = tc, bufio.NewReader(tc), tc, true
		case cmd == "AUTH":
			m.auth = line
			say("235 ok")
		case cmd == "MAIL":
			if atomic.AddInt32(&f.failMail, -1) >= 0 {
				say("451 try later")
				continue
			}
			m.from = line
			say("250 ok")
		case cmd == "RCPT" && strings.Contains(line, "bad@"):
			say("550 no such user")
		case cmd == "RCPT":
			m.to = append(m.to, line)
			say("250 ok")
		case cmd == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			m.data = b.String()
			f.got <- m
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) mailer(t *testing.T) *mailer {
	m := newMailer(&Config{
		SMTPHost: "127.0.0.1", SMTPPort: f.ln.Addr().(*net.TCPAddr).Port,
		SMTPUser: "bot", SMTPPassword: "pw", SMTPFrom: "NoteTaker <notetaker@example.com>",
	})
	m.retries = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	m.tlsConfig = &tls.Config{ServerName: "127.0.0.1", RootCAs: f.roots}
	return m
}

func (f *fakeSMTP) wait(t *testing.T) fakeMsg {
	t.Helper()
	select {
	case m := <-f.got:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message delivered")
		return fakeMsg{}
	}
}

func TestMailHeaders(t *testing.T) {
	f := newFakeSMTP(t, 0)
	f.mailer(t).mailLobby("job-1", []string{"a@example.com", "b@example.com"}, "Plan\r\nBcc: x@example.com")
	got := f.wait(t)

	if !got.tls {
		t.Error("no STARTTLS")
	}
	if got.auth != "AUTH PLAIN "+base64.StdEncoding.EncodeToString([]byte("\x00bot\x00pw")) {
		t.Errorf("auth = %q", got.auth)
	}
	if got.from != "MAIL FROM:<notetaker@example.com>" || len(got.to) != 2 {
		t.Errorf("envelope = %q %q", got.from, got.to)
	}
	msg, err := mail.ReadMessage(strings.NewReader(got.data))
	if err != nil {
		t.Fatal(err)
	}
	h := msg.Header
	for k, want := range map[string]string{
		"From":         "NoteTaker <notetaker@example.com>",
		"To":           "a@example.com, b@example.com",
		"Subject":      "NoteTaker is waiting in the lobby: Plan Bcc: x@example.com",
		"Mime-Version": "1.0",
		"Content-Type": "text/plain; charset=utf-8",
	} {
		if h.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, h.Get(k), want)
		}
	}
	if h.Get("Bcc") != "" {
		t.Error("header injection through the title")
	}
	if _, err := h.Date(); err != nil {
		t.Errorf("Date: %v", err)
	}
	if id := h.Get("Message-Id"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
		t.Errorf("Message-ID = %q", id)
	}
}

func TestMailEncodedSubject(t *testing.T) {
	f := newFakeSMTP(t, 0)
	long := strings.Repeat("Ä word ", 500) // one 3500-byte line
	f.mailer(t).mailTranscript("job-2", []string{"a@example.com"}, "Встреча über alles", "line 1\n.\n"+long+"\n")
	got := f.wait(t)
	for _, l := range strings.Split(got.data, "\r\n") {
		if len(l) > 998 {
			t.Fatalf("line of %d bytes on the wire", len(l))
		}
	}
	msg, err := mail.ReadMessage(strings.NewReader(got.data))
	if err != nil {
		t.Fatal(err)
	}
	raw := msg.Header.Get("Subject")
	if !strings.HasPrefix(raw, "=?utf-8?q?") {
		t.Errorf("subject not encoded: %q", raw)
	}
	if s, err := new(mime.WordDecoder).DecodeHeader(raw); err != nil || s != "Transcript ready: Встреча über alles" {
		t.Errorf("decoded subject = %q, %v", s, err)
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if want := "line 1\r\n..\r\n" + long + "\r\n"; string(body) != want { // ".." : still dot-stuffed on the wire
		t.Errorf("body = %q", body)
	}
}

func TestMailRetryAfter4xx(t *testing.T) {
	f := newFakeSMTP(t, 2) // two 451s, then success on the last retry
	f.mailer(t).mailFailed("job-3", []string{"a@example.com"}, "Sync", "lobby timeout")
	got := f.wait(t)
	if !strings.Contains(got.data, "Subject: Recording failed: Sync (lobby timeout)") {
		t.Errorf("data = %q", got.data)
	}
}

func TestMailGivesUpAfterRetries(t *testing.T) {
	f := newFakeSMTP(t, 3)
	f.mailer(t).mailTooShort("job-4", []string{"a@example.com"}, "Sync")
	select {
	case <-f.got:
		t.Fatal("delivered after more failures than retries")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestMailRejectedRecipientSkipped(t *testing.T) {
	f := newFakeSMTP(t, 0)
	f.mailer(t).mailLobby("job-5", []string{"bad@example.com", "a@example.com"}, "Sync")
	if got := f.wait(t); len(got.to) != 1 || !strings.Contains(got.to[0], "a@example.com") {
		t.Errorf("recipients = %q", got.to)
	}
}

func TestMailDisabled(t *testing.T) {
	m := newMailer(&Config{SMTPPort: 587})
	m.mailDisconnected("hash", "a@example.com") // must return without dialing anything
	if m.host != "" {
		t.Fatal("mailer enabled without SMTP_HOST")
	}
}
