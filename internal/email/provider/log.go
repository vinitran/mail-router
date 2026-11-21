package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"strings"

	"mail-router/internal/email"
)

// LogName (EMAIL_PROVIDER=log) logs message metadata instead of sending,
// for local development. NoopName (EMAIL_PROVIDER=noop) drops messages
// silently. Neither logs bodies or full recipient addresses.
const (
	LogName  = "log"
	NoopName = "noop"
)

func init() {
	email.Register(LogName, func(email.Env) (email.Sender, error) { return NewLog(log.Printf), nil })
	email.Register(NoopName, func(email.Env) (email.Sender, error) { return NewLog(nil), nil })
}

// NewLog returns a sender that reports metadata through logf (nil: silent).
func NewLog(logf func(format string, args ...any)) *Log {
	name := LogName
	if logf == nil {
		name = NoopName
		logf = func(string, ...any) {}
	}
	return &Log{name: name, logf: logf}
}

// Log is the log/noop provider.
type Log struct {
	name string
	logf func(format string, args ...any)
}

// Send implements email.Sender.
func (l *Log) Send(ctx context.Context, msg email.Message) (email.Result, error) {
	result := email.Result{Provider: l.name}
	if err := ctx.Err(); err != nil {
		return result, email.Classify(l.name, email.ErrTemporary, err)
	}
	var id [8]byte
	_, _ = rand.Read(id[:])
	result.ProviderMessageID = l.name + "-" + hex.EncodeToString(id[:])
	masked := make([]string, 0, len(msg.To))
	for _, to := range msg.To {
		masked = append(masked, MaskAddress(to.Address))
	}
	l.logf("email[%s]: id=%s category=%q to=%v subject_len=%d html_bytes=%d text_bytes=%d idempotency_key_set=%t",
		l.name, result.ProviderMessageID, msg.Category, masked, len(msg.Subject), len(msg.HTML), len(msg.Text), msg.IdempotencyKey != "")
	return result, nil
}

// MaskAddress keeps the first character and the domain: "a***@gmail.com".
func MaskAddress(address string) string {
	at := strings.LastIndex(address, "@")
	if at <= 0 {
		return "***"
	}
	return address[:1] + "***" + address[at:]
}
