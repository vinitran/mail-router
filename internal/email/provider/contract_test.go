package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"mail-router/internal/email"
)

// captured is what a fake transport received, in provider-neutral form.
type captured struct {
	From, To, Subject, HTML, Text string
}

// harness adapts one provider and its fake transport to the contract.
type harness struct {
	name   string
	sender email.Sender
	// last returns the message the transport received; nil when the
	// provider has no transport (log/noop).
	last func(t *testing.T) (captured, bool)
	// failNext makes the transport answer the next send with its native
	// signal for class; nil when the provider cannot fail.
	failNext func(class error)
}

func contractMessage() email.Message {
	return email.Message{
		From:     email.Address{Name: "Hoàn Xu", Address: "noreply@hoanxu.com"},
		To:       email.To("user@example.com"),
		Subject:  "Hoàn tiền thành công — HoanXu.com",
		HTML:     "<p>Xin chào <strong>bạn</strong></p>",
		Text:     "Xin chào bạn",
		Category: "cashback_paid",
	}
}

// runContract is the behaviour every provider must share (Liskov): the
// router, decorators and the HTTP API rely on exactly this.
func runContract(t *testing.T, newHarness func(t *testing.T) harness) {
	t.Run("delivers the message and reports itself", func(t *testing.T) {
		h := newHarness(t)
		result, err := h.sender.Send(context.Background(), contractMessage())
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		if result.Provider != h.name || result.ProviderMessageID == "" {
			t.Fatalf("result %+v, want provider %q and a message ID", result, h.name)
		}
		if h.last == nil {
			return
		}
		got, ok := h.last(t)
		if !ok {
			t.Fatal("transport received nothing")
		}
		msg := contractMessage()
		if !strings.Contains(got.To, "user@example.com") || !strings.Contains(got.From, "noreply@hoanxu.com") {
			t.Errorf("addresses: from %q to %q", got.From, got.To)
		}
		if !strings.Contains(got.Subject, "Ho") || !strings.Contains(got.HTML, msg.HTML) {
			t.Errorf("content: subject %q html %q", got.Subject, got.HTML)
		}
	})

	t.Run("a cancelled context fails with a classified error and sends nothing", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := h.sender.Send(ctx, contractMessage())
		if err == nil || email.ClassOf(err) == nil {
			t.Fatalf("err = %v, want a classified error", err)
		}
		if h.last != nil {
			if _, ok := h.last(t); ok {
				t.Fatal("a cancelled send reached the transport")
			}
		}
	})

	for _, class := range []error{email.ErrRateLimited, email.ErrQuotaExceeded, email.ErrInvalidRecipient} {
		t.Run("maps native "+email.ClassName(class), func(t *testing.T) {
			h := newHarness(t)
			if h.failNext == nil {
				t.Skip("provider has no transport failures")
			}
			h.failNext(class)
			result, err := h.sender.Send(context.Background(), contractMessage())
			if !errors.Is(err, class) {
				t.Fatalf("err = %v (class %s), want %s", err, email.ClassName(err), email.ClassName(class))
			}
			if result.Provider != h.name {
				t.Fatalf("failed result must still name the provider, got %+v", result)
			}
			var classified *email.Error
			if !errors.As(err, &classified) || classified.Provider != h.name {
				t.Fatalf("error must carry the provider name: %#v", err)
			}
		})
	}
}

func TestContractSMTP(t *testing.T) {
	runContract(t, func(t *testing.T) harness {
		server := newFakeSMTPServer(t)
		return harness{
			name:   SMTPName,
			sender: server.provider(t, false),
			last: func(*testing.T) (captured, bool) {
				s, ok := server.last()
				if !ok {
					return captured{}, false
				}
				headers, body := readHeaders(s.Data)
				c := captured{To: strings.Join(s.Rcpt, ","), From: s.From, HTML: body}
				for _, h := range headers {
					if v, ok := strings.CutPrefix(h, "Subject: "); ok {
						c.Subject = v
					}
				}
				return c, true
			},
			failNext: func(class error) {
				switch class {
				case email.ErrRateLimited:
					server.set("", "451 4.7.1 Rate limit exceeded, try again later", "")
				case email.ErrQuotaExceeded:
					server.set("550 5.7.0 Daily sending quota exceeded", "", "")
				case email.ErrInvalidRecipient:
					server.set("", "550 5.1.1 No such user here", "")
				}
			},
		}
	})
}

func TestContractResend(t *testing.T) {
	runContract(t, func(t *testing.T) harness {
		api := newFakeResendAPI(t)
		return harness{
			name:   ResendName,
			sender: api.resend(t),
			last: func(*testing.T) (captured, bool) {
				_, body, ok := api.last()
				if !ok {
					return captured{}, false
				}
				to, _ := body["to"].([]any)
				c := captured{From: str(body["from"]), Subject: str(body["subject"]), HTML: str(body["html"]), Text: str(body["text"])}
				for _, v := range to {
					c.To += str(v)
				}
				return c, true
			},
			failNext: func(class error) {
				switch class {
				case email.ErrRateLimited:
					api.respond(429, `{"statusCode":429,"name":"rate_limit_exceeded","message":"Too many requests."}`)
				case email.ErrQuotaExceeded:
					api.respond(429, `{"statusCode":429,"name":"daily_quota_exceeded","message":"You have reached your daily email sending quota."}`)
				case email.ErrInvalidRecipient:
					api.respond(422, `{"statusCode":422,"name":"validation_error","message":"Invalid `+"`to`"+` field."}`)
				}
			},
		}
	})
}

func TestContractSES(t *testing.T) {
	runContract(t, func(t *testing.T) harness {
		client := &fakeSESClient{}
		sender, err := NewSES(client, SESConfig{Region: "ap-southeast-1"})
		if err != nil {
			t.Fatal(err)
		}
		return harness{
			name:   SESName,
			sender: sender,
			last: func(*testing.T) (captured, bool) {
				in, ok := client.last()
				if !ok {
					return captured{}, false
				}
				simple := in.Content.Simple
				return captured{
					From: *in.FromEmailAddress, To: strings.Join(in.Destination.ToAddresses, ","),
					Subject: *simple.Subject.Data, HTML: *simple.Body.Html.Data, Text: *simple.Body.Text.Data,
				}, true
			},
			failNext: func(class error) {
				switch class {
				case email.ErrRateLimited:
					client.fail("TooManyRequestsException", "Too many requests")
				case email.ErrQuotaExceeded:
					client.fail("LimitExceededException", "Daily message quota exceeded")
				case email.ErrInvalidRecipient:
					client.fail("MessageRejected", "Email address is not verified.")
				}
			},
		}
	})
}

func TestContractLogAndNoop(t *testing.T) {
	for _, name := range []string{LogName, NoopName} {
		t.Run(name, func(t *testing.T) {
			runContract(t, func(t *testing.T) harness {
				sender, err := email.NewProvider(name, nil)
				if err != nil {
					t.Fatal(err)
				}
				return harness{name: name, sender: sender}
			})
		})
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
