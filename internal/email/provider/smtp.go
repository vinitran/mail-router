package provider

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"mail-router/internal/email"
)

// SMTPName selects any SMTP relay (EMAIL_PROVIDER=smtp, the default). This
// is the transport the API used before providers became pluggable; the
// message it writes is unchanged.
const SMTPName = "smtp"

func init() { email.Register(SMTPName, newSMTPFromEnv) }

// SMTPConfig is read from EMAIL_SMTP_*.
type SMTPConfig struct {
	Host     string // EMAIL_SMTP_HOST, default smtp.privateemail.com
	Port     int    // EMAIL_SMTP_PORT, default 587 (STARTTLS); 465 = implicit TLS
	Username string // EMAIL_SMTP_USERNAME
	Password string // EMAIL_SMTP_PASSWORD
	// TextPart sends multipart/alternative with the plain-text part
	// (EMAIL_SMTP_TEXT_PART). Off by default: the SMTP transport has always
	// sent a single text/html part.
	TextPart bool
}

func newSMTPFromEnv(env email.Env) (email.Sender, error) {
	port, err := env.Int("EMAIL_SMTP_PORT", 587)
	if err != nil {
		// As before: a malformed port falls back to the default.
		log.Printf("Warning: %v, using default: 587", err)
	}
	textPart, err := env.Bool("EMAIL_SMTP_TEXT_PART", false)
	if err != nil {
		return nil, err
	}
	return NewSMTP(SMTPConfig{
		Host:     env.String("EMAIL_SMTP_HOST", "smtp.privateemail.com"),
		Port:     port,
		Username: env.String("EMAIL_SMTP_USERNAME", ""),
		Password: env.String("EMAIL_SMTP_PASSWORD", ""),
		TextPart: textPart,
	})
}

// NewSMTP validates cfg and returns the SMTP provider.
func NewSMTP(cfg SMTPConfig) (*SMTP, error) {
	var missing []string
	for key, value := range map[string]string{"EMAIL_SMTP_HOST": cfg.Host, "EMAIL_SMTP_USERNAME": cfg.Username, "EMAIL_SMTP_PASSWORD": cfg.Password} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%s required", strings.Join(missing, ", "))
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("EMAIL_SMTP_PORT %d is not a valid port", cfg.Port)
	}
	return &SMTP{cfg: cfg, now: time.Now}, nil
}

// SMTP sends through an SMTP relay with STARTTLS (or implicit TLS on 465)
// and PLAIN auth.
type SMTP struct {
	cfg     SMTPConfig
	now     func() time.Time
	rootCAs *x509.CertPool // tests only; nil = system roots
}

// Send implements email.Sender.
func (s *SMTP) Send(ctx context.Context, msg email.Message) (email.Result, error) {
	result := email.Result{Provider: SMTPName}
	if err := ctx.Err(); err != nil {
		return result, email.Classify(SMTPName, email.ErrTemporary, err)
	}
	fromEmail := msg.From.Address
	if fromEmail == "" {
		fromEmail = s.cfg.Username
	}
	message, messageID := s.build(msg, fromEmail)
	result.ProviderMessageID = messageID

	// Setup SMTP connection
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)

	// Create TLS config
	tlsConfig := &tls.Config{
		InsecureSkipVerify: false,
		ServerName:         s.cfg.Host,
		RootCAs:            s.rootCAs,
	}

	// Bound dial, TLS and SMTP I/O by context so scheduled mail cannot hang or
	// continue indefinitely after its delivery window closes.
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return result, s.fail("", fmt.Errorf("connect SMTP server: %w", err))
	}
	defer conn.Close()
	deadline := time.Now().Add(30 * time.Second)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return result, s.fail("", fmt.Errorf("set SMTP deadline: %w", err))
	}
	rawConn := conn
	stopCancel := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stopCancel()
	if s.cfg.Port == 465 {
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return result, s.fail("", fmt.Errorf("SMTP TLS handshake: %w", err))
		}
		conn = tlsConn
	}
	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return result, s.fail("", fmt.Errorf("create SMTP client: %w", err))
	}
	defer client.Close()
	if s.cfg.Port != 465 {
		if err := client.StartTLS(tlsConfig); err != nil {
			client.Close()
			return result, s.fail("", fmt.Errorf("failed to start TLS: %w", err))
		}
	}

	// Authenticate
	auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	if err := client.Auth(auth); err != nil {
		return result, s.fail("auth", fmt.Errorf("failed to authenticate: %w", err))
	}

	// Set sender
	if err := client.Mail(fromEmail); err != nil {
		return result, s.fail("mail", fmt.Errorf("failed to set sender: %w", err))
	}

	// Set recipients
	for _, recipient := range msg.Recipients() {
		if err := client.Rcpt(recipient); err != nil {
			return result, s.fail("rcpt", fmt.Errorf("failed to set recipient %s: %w", recipient, err))
		}
	}

	// Send email body
	writer, err := client.Data()
	if err != nil {
		return result, s.fail("data", fmt.Errorf("failed to get data writer: %w", err))
	}

	if _, err := writer.Write([]byte(message)); err != nil {
		writer.Close()
		return result, s.fail("data", fmt.Errorf("failed to write message: %w", err))
	}

	if err := writer.Close(); err != nil {
		return result, s.fail("data", fmt.Errorf("failed to close writer: %w", err))
	}

	// Quit
	if err := client.Quit(); err != nil {
		return result, s.fail("quit", fmt.Errorf("failed to quit: %w", err))
	}

	return result, nil
}

// build writes the RFC 5322 message exactly as the SMTP-only service did:
// same headers in the same order, a single text/html (or text/plain) part.
func (s *SMTP) build(msg email.Message, fromEmail string) (string, string) {
	fromName := msg.From.Name
	if fromName == "" {
		fromName = email.DefaultFromName
	}
	replyTo := fromEmail
	if !msg.ReplyTo.IsZero() {
		replyTo = formatSMTPAddress(msg.ReplyTo)
	}
	to := make([]string, 0, len(msg.To))
	for _, address := range msg.To {
		to = append(to, formatSMTPAddress(address))
	}
	now := s.now()

	// Build headers with anti-spam best practices
	headers := make([]string, 0, 16)

	// Required headers
	headers = append(headers, fmt.Sprintf("From: %s <%s>", fromName, fromEmail))
	headers = append(headers, fmt.Sprintf("To: %s", strings.Join(to, ", ")))
	headers = append(headers, fmt.Sprintf("Reply-To: %s", replyTo))
	headers = append(headers, fmt.Sprintf("Subject: %s", msg.Subject))
	headers = append(headers, fmt.Sprintf("Date: %s", now.Format(time.RFC1123Z)))

	// Message-ID for better deliverability
	messageID := fmt.Sprintf("<%d.%s@%s>", now.UnixNano(), strings.ReplaceAll(fromEmail, "@", "."), s.cfg.Host)
	headers = append(headers, fmt.Sprintf("Message-ID: %s", messageID))

	// MIME headers
	headers = append(headers, "MIME-Version: 1.0")

	body := msg.HTML
	contentType := "text/html; charset=UTF-8"
	multipart := s.cfg.TextPart && msg.HTML != "" && msg.Text != ""
	switch {
	case multipart:
		boundary := randomBoundary()
		contentType = fmt.Sprintf("multipart/alternative; boundary=%q", boundary)
		body = multipartAlternative(boundary, msg.Text, msg.HTML)
	case msg.HTML == "":
		contentType = "text/plain; charset=UTF-8"
		body = msg.Text
	}
	headers = append(headers, fmt.Sprintf("Content-Type: %s", contentType))

	// Anti-spam headers
	headers = append(headers, "X-Mailer: HoanXu.com Email Service")
	headers = append(headers, "X-Priority: 3")    // Normal priority
	headers = append(headers, "Precedence: bulk") // For transactional emails

	// Feature headers (e.g. List-Unsubscribe), in a stable order.
	keys := make([]string, 0, len(msg.Headers))
	for key := range msg.Headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		headers = append(headers, fmt.Sprintf("%s: %s", textproto.CanonicalMIMEHeaderKey(key), msg.Headers[key]))
	}

	return strings.Join(headers, "\r\n") + "\r\n\r\n" + body, messageID
}

// formatSMTPAddress keeps a bare address bare (as before) and encodes a
// display name per RFC 5322 / 2047.
func formatSMTPAddress(a email.Address) string {
	if a.Name == "" {
		return a.Address
	}
	return (&mail.Address{Name: a.Name, Address: a.Address}).String()
}

func randomBoundary() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:])
	return "hoanxu-" + hex.EncodeToString(buf[:])
}

func multipartAlternative(boundary, text, html string) string {
	var b strings.Builder
	for _, part := range []struct{ contentType, body string }{{"text/plain; charset=UTF-8", text}, {"text/html; charset=UTF-8", html}} {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + part.contentType + "\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		w := quotedprintable.NewWriter(&b)
		_, _ = w.Write([]byte(part.body))
		_ = w.Close()
		b.WriteString("\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

// fail classifies err for retry/failover. The message text is unchanged.
func (s *SMTP) fail(stage string, err error) error {
	return email.Classify(SMTPName, classifySMTP(stage, err), err)
}

func classifySMTP(stage string, err error) error {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		text := strings.ToLower(protoErr.Msg)
		switch {
		case strings.Contains(text, "quota") || strings.Contains(text, "daily") || strings.Contains(text, "limit exceeded for the day"):
			return email.ErrQuotaExceeded
		case strings.Contains(text, "rate limit") || strings.Contains(text, "rate-limit") || strings.Contains(text, "ratelimit") ||
			strings.Contains(text, "too many") || strings.Contains(text, "throttl"):
			return email.ErrRateLimited
		case protoErr.Code >= 400 && protoErr.Code < 500:
			return email.ErrTemporary
		case stage == "rcpt" && (protoErr.Code == 550 || protoErr.Code == 551 || protoErr.Code == 553 || protoErr.Code == 501):
			return email.ErrInvalidRecipient
		default:
			return email.ErrPermanent
		}
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) || stage == "auth" {
		return email.ErrPermanent
	}
	// Dial, TLS, timeouts and dropped connections.
	return email.ErrTemporary
}
