// Package email delivers ready-made messages through email providers.
//
//   - transport: a Sender delivers a ready Message. Providers (smtp, resend,
//     brevo, sendgrid, ses, log, noop) live in the provider package and
//     register themselves with Register;
//   - cross-cutting concerns: Retry, RateLimit, Router and Instrument are
//     Sender decorators, each in its own file;
//   - Build turns Settings into the Sender chain the HTTP server uses, and
//     Validate rejects messages no provider should send.
//
// Composing messages (templates) stays with the callers, such as api-core.
// See README.md at the repository root.
package email

import (
	"context"
	"strings"
)

// Address is a mailbox. Name is optional.
type Address struct {
	Name    string
	Address string
}

// IsZero reports whether no address is set.
func (a Address) IsZero() bool { return strings.TrimSpace(a.Address) == "" }

// Message is a provider-neutral, ready-to-send email.
type Message struct {
	// From defaults to EMAIL_FROM_NAME / EMAIL_FROM_ADDRESS of the
	// mail-router when the caller leaves it empty.
	From    Address
	To      []Address
	ReplyTo Address // optional; providers default it as they did before

	Subject string
	HTML    string
	Text    string // plain-text alternative; optional

	// Headers are extra MIME headers, e.g. List-Unsubscribe.
	Headers map[string]string
	// Category groups mail for provider analytics (Resend tag, SES tag).
	Category string
	// Tags are extra provider analytics tags (name -> value).
	Tags map[string]string
	// IdempotencyKey lets providers that support it (Resend) drop duplicate
	// submissions of the same logical email, e.g. on a Kafka redelivery.
	IdempotencyKey string
}

// Recipients returns the bare To addresses.
func (m Message) Recipients() []string {
	out := make([]string, 0, len(m.To))
	for _, to := range m.To {
		out = append(out, to.Address)
	}
	return out
}

// Result describes an accepted message.
type Result struct {
	Provider          string
	ProviderMessageID string
}

// Sender delivers a ready message. Every provider and every decorator
// implements it.
type Sender interface {
	Send(ctx context.Context, msg Message) (Result, error)
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, msg Message) (Result, error)

func (f SenderFunc) Send(ctx context.Context, msg Message) (Result, error) { return f(ctx, msg) }

// To is a convenience for a single bare recipient.
func To(address string) []Address { return []Address{{Address: address}} }
