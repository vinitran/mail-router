package provider

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"mail-router/internal/email"
)

// TestSMTPWritesTheSameMessageAsBefore pins the wire format of the
// pre-refactor SMTP service (headers, order, single text/html part).
func TestSMTPWritesTheSameMessageAsBefore(t *testing.T) {
	server := newFakeSMTPServer(t)
	p := server.provider(t, false)
	fixed := time.Date(2026, 11, 13, 22, 0, 0, 123, time.FixedZone("ICT", 7*3600))
	p.now = func() time.Time { return fixed }
	msg := contractMessage()
	msg.Text = "ignored unless EMAIL_SMTP_TEXT_PART=true"
	result, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := server.last()
	messageID := "<" + strconv.FormatInt(fixed.UnixNano(), 10) + ".noreply.hoanxu.com@127.0.0.1>"
	want := strings.Join([]string{
		"From: Hoàn Xu <noreply@hoanxu.com>",
		"To: user@example.com",
		"Reply-To: noreply@hoanxu.com",
		"Subject: Hoàn tiền thành công — HoanXu.com",
		"Date: Fri, 13 Nov 2026 22:00:00 +0700",
		"Message-ID: " + messageID,
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=UTF-8",
		"X-Mailer: HoanXu.com Email Service",
		"X-Priority: 3",
		"Precedence: bulk",
		"",
		"<p>Xin chào <strong>bạn</strong></p>",
	}, "\n")
	if strings.TrimRight(session.Data, "\n") != want {
		t.Fatalf("message changed:\n%s\nwant:\n%s", session.Data, want)
	}
	if session.From != "noreply@hoanxu.com" || strings.Join(session.Rcpt, ",") != "user@example.com" {
		t.Fatalf("envelope %+v", session)
	}
	if result.ProviderMessageID != messageID {
		t.Fatalf("message ID %q", result.ProviderMessageID)
	}
}

func TestSMTPFallsBackToUsernameAndAddsFeatureHeaders(t *testing.T) {
	server := newFakeSMTPServer(t)
	p := server.provider(t, false)
	msg := contractMessage()
	msg.From = email.Address{}
	msg.ReplyTo = email.Address{Name: "Hỗ trợ", Address: "support@hoanxu.com"}
	msg.Headers = map[string]string{"list-unsubscribe": "<https://hoanxu.com/unsubscribe>"}
	if _, err := p.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	session, _ := server.last()
	headers, _ := readHeaders(session.Data)
	joined := strings.Join(headers, "\n")
	for _, want := range []string{
		"From: Hoàn Xu <relay-user@hoanxu.test>",
		"Reply-To: =?utf-8?q?H=E1=BB=97_tr=E1=BB=A3?= <support@hoanxu.com>",
		"Precedence: bulk\nList-Unsubscribe: <https://hoanxu.com/unsubscribe>",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	if session.From != "relay-user@hoanxu.test" {
		t.Fatalf("envelope sender %q", session.From)
	}
}

func TestSMTPMultipartWhenEnabled(t *testing.T) {
	server := newFakeSMTPServer(t)
	if _, err := server.provider(t, true).Send(context.Background(), contractMessage()); err != nil {
		t.Fatal(err)
	}
	session, _ := server.last()
	for _, want := range []string{"Content-Type: multipart/alternative; boundary=", "Content-Type: text/plain; charset=UTF-8", "Content-Type: text/html; charset=UTF-8", "Xin ch=C3=A0o b=E1=BA=A1n"} {
		if !strings.Contains(session.Data, want) {
			t.Errorf("multipart message lacks %q", want)
		}
	}
}

func TestSMTPErrorsKeepTheirKindAndText(t *testing.T) {
	server := newFakeSMTPServer(t)
	server.set("", "", "535 5.7.8 Authentication credentials invalid")
	_, err := server.provider(t, false).Send(context.Background(), contractMessage())
	if !strings.HasPrefix(err.Error(), "failed to authenticate: ") || !errors.Is(err, email.ErrPermanent) {
		t.Fatalf("err = %v (%s)", err, email.ClassName(err))
	}
	server.set("", "421 4.3.2 Service shutting down", "")
	if _, err := server.provider(t, false).Send(context.Background(), contractMessage()); !errors.Is(err, email.ErrTemporary) {
		t.Fatalf("4xx must be temporary: %v", err)
	}
}

func TestSMTPConnectFailureIsTemporary(t *testing.T) {
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	n, _ := strconv.Atoi(port)
	p, _ := NewSMTP(SMTPConfig{Host: "127.0.0.1", Port: n, Username: "u", Password: "p"})
	_, err := p.Send(context.Background(), contractMessage())
	if !errors.Is(err, email.ErrTemporary) || !strings.HasPrefix(err.Error(), "connect SMTP server: ") {
		t.Fatalf("err = %v", err)
	}
}

// Ported from the SMTP-only service: cancellation must unblock a server
// that never greets.
func TestSMTPHonorsCancellationWhileWaitingForGreeting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		cancel()
		// Keep the server side open: only client context cancellation may unblock SMTP.
		<-release
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	p, err := NewSMTP(SMTPConfig{Host: host, Port: portNumber, Username: "local-test", Password: "local-test"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := p.Send(ctx, contractMessage()); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected SMTP cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("SMTP ignored cancellation")
	}
}
