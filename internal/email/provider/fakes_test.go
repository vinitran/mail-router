package provider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/smithy-go"
)

// ---- fake SMTP server (STARTTLS + AUTH PLAIN), no network beyond loopback.

type smtpSession struct {
	From string
	Rcpt []string
	Data string
}

type fakeSMTPServer struct {
	t        *testing.T
	listener net.Listener
	tls      *tls.Config
	roots    *x509.CertPool

	mu        sync.Mutex
	sessions  []smtpSession
	mailReply string // e.g. "550 5.7.0 Daily sending quota exceeded"
	rcptReply string // e.g. "451 4.7.1 Rate limit exceeded"
	authReply string
}

func newFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	cert, roots := selfSignedLoopbackCert(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTPServer{t: t, listener: listener, tls: &tls.Config{Certificates: []tls.Certificate{cert}}, roots: roots}
	t.Cleanup(func() { _ = listener.Close() })
	go s.serve()
	return s
}

func (s *fakeSMTPServer) port() int {
	_, port, _ := net.SplitHostPort(s.listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return n
}

// provider returns an SMTP provider pointed at the fake server.
func (s *fakeSMTPServer) provider(t *testing.T, textPart bool) *SMTP {
	t.Helper()
	p, err := NewSMTP(SMTPConfig{Host: "127.0.0.1", Port: s.port(), Username: "relay-user@hoanxu.test", Password: "local-test", TextPart: textPart})
	if err != nil {
		t.Fatal(err)
	}
	p.rootCAs = s.roots
	return p
}

func (s *fakeSMTPServer) set(mail, rcpt, auth string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailReply, s.rcptReply, s.authReply = mail, rcpt, auth
}

func (s *fakeSMTPServer) last() (smtpSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) == 0 {
		return smtpSession{}, false
	}
	return s.sessions[len(s.sessions)-1], true
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	tp := textproto.NewConn(conn)
	reply := func(line string) { _ = tp.PrintfLine("%s", line) }
	reply("220 fake ESMTP")
	secure := false
	var session smtpSession
	s.mu.Lock()
	mailReply, rcptReply, authReply := s.mailReply, s.rcptReply, s.authReply
	s.mu.Unlock()
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
		case "EHLO", "HELO":
			if secure {
				reply("250-fake\r\n250 AUTH PLAIN")
			} else {
				reply("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN")
			}
		case "STARTTLS":
			reply("220 ready")
			tlsConn := tls.Server(conn, s.tls)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			tp = textproto.NewConn(tlsConn)
			secure = true
		case "AUTH":
			if authReply != "" {
				reply(authReply)
				continue
			}
			reply("235 ok")
		case "MAIL":
			if mailReply != "" {
				reply(mailReply)
				continue
			}
			session.From = strings.TrimSuffix(strings.TrimPrefix(line[len("MAIL FROM:"):], "<"), ">")
			reply("250 ok")
		case "RCPT":
			if rcptReply != "" {
				reply(rcptReply)
				continue
			}
			session.Rcpt = append(session.Rcpt, strings.TrimSuffix(strings.TrimPrefix(line[len("RCPT TO:"):], "<"), ">"))
			reply("250 ok")
		case "DATA":
			reply("354 go ahead")
			data, err := io.ReadAll(tp.DotReader())
			if err != nil {
				return
			}
			session.Data = string(data)
			s.mu.Lock()
			s.sessions = append(s.sessions, session)
			s.mu.Unlock()
			reply("250 queued")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not implemented")
		}
	}
}

func selfSignedLoopbackCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := x509.ParseCertificate(der)
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

// ---- fake JSON HTTP API (Resend, Brevo, SendGrid).

type fakeHTTPAPI struct {
	server *httptest.Server

	mu          sync.Mutex
	requests    []*http.Request
	bodies      []map[string]any
	status      int
	reply       string
	respHeaders map[string]string
}

// fakeResendAPI keeps the name the Resend tests use.
type fakeResendAPI = fakeHTTPAPI

func newFakeResendAPI(t *testing.T) *fakeHTTPAPI {
	return newFakeHTTPAPI(t, http.StatusOK, `{"id":"re_123"}`, nil)
}

func newFakeHTTPAPI(t *testing.T, status int, reply string, respHeaders map[string]string) *fakeHTTPAPI {
	f := &fakeHTTPAPI{status: status, reply: reply, respHeaders: respHeaders}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.bodies = append(f.bodies, body)
		status, reply := f.status, f.reply
		for k, v := range f.respHeaders {
			w.Header().Set(k, v)
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeHTTPAPI) resend(t *testing.T) *Resend {
	t.Helper()
	p, err := NewResend(ResendConfig{APIKey: "re_test_key", BaseURL: f.server.URL}, f.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fakeHTTPAPI) respond(status int, reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.reply = status, reply
}

func (f *fakeHTTPAPI) last() (*http.Request, map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil, nil, false
	}
	return f.requests[len(f.requests)-1], f.bodies[len(f.bodies)-1], true
}

// ---- fake SES v2 client.

type fakeSESClient struct {
	mu     sync.Mutex
	inputs []*sesv2.SendEmailInput
	err    error
}

func (f *fakeSESClient) SendEmail(ctx context.Context, in *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.inputs = append(f.inputs, in)
	id := "ses-" + strconv.Itoa(len(f.inputs))
	return &sesv2.SendEmailOutput{MessageId: &id}, nil
}

func (f *fakeSESClient) fail(code, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code == "" {
		f.err = nil
		return
	}
	f.err = &smithy.GenericAPIError{Code: code, Message: message}
}

func (f *fakeSESClient) last() (*sesv2.SendEmailInput, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		return nil, false
	}
	return f.inputs[len(f.inputs)-1], true
}

// readHeaders splits captured DATA into header lines and body.
func readHeaders(data string) ([]string, string) {
	head, body, _ := strings.Cut(data, "\n\n")
	return strings.Split(head, "\n"), body
}
