package provider

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"mail-router/internal/email"
)

func newFakeBrevoAPI(t *testing.T) (*fakeHTTPAPI, *Brevo) {
	api := newFakeHTTPAPI(t, http.StatusCreated, `{"messageId":"<202611210800.123@smtp-relay.mailin.fr>"}`, nil)
	p, err := NewBrevo(BrevoConfig{APIKey: "xkeysib-test", BaseURL: api.server.URL}, api.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return api, p
}

func newFakeSendGridAPI(t *testing.T) (*fakeHTTPAPI, *SendGrid) {
	api := newFakeHTTPAPI(t, http.StatusAccepted, ``, map[string]string{"X-Message-Id": "sg_123"})
	p, err := NewSendGrid(SendGridConfig{APIKey: "SG.test", BaseURL: api.server.URL}, api.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return api, p
}

func TestContractBrevo(t *testing.T) {
	runContract(t, func(t *testing.T) harness {
		api, p := newFakeBrevoAPI(t)
		return harness{
			name:   BrevoName,
			sender: p,
			last: func(*testing.T) (captured, bool) {
				_, body, ok := api.last()
				if !ok {
					return captured{}, false
				}
				sender, _ := body["sender"].(map[string]any)
				c := captured{From: str(sender["email"]), Subject: str(body["subject"]), HTML: str(body["htmlContent"]), Text: str(body["textContent"])}
				to, _ := body["to"].([]any)
				for _, v := range to {
					m, _ := v.(map[string]any)
					c.To += str(m["email"])
				}
				return c, true
			},
			failNext: func(class error) {
				switch class {
				case email.ErrRateLimited:
					api.respond(429, `{"code":"too_many_requests","message":"Too many requests"}`)
				case email.ErrQuotaExceeded:
					api.respond(402, `{"code":"not_enough_credits","message":"You don't have enough credits to send this email"}`)
				case email.ErrInvalidRecipient:
					api.respond(400, `{"code":"invalid_parameter","message":"email is not valid in to"}`)
				}
			},
		}
	})
}

func TestContractSendGrid(t *testing.T) {
	runContract(t, func(t *testing.T) harness {
		api, p := newFakeSendGridAPI(t)
		return harness{
			name:   SendGridName,
			sender: p,
			last: func(*testing.T) (captured, bool) {
				_, body, ok := api.last()
				if !ok {
					return captured{}, false
				}
				from, _ := body["from"].(map[string]any)
				c := captured{From: str(from["email"]), Subject: str(body["subject"])}
				pers, _ := body["personalizations"].([]any)
				for _, p := range pers {
					to, _ := p.(map[string]any)["to"].([]any)
					for _, v := range to {
						c.To += str(v.(map[string]any)["email"])
					}
				}
				content, _ := body["content"].([]any)
				for _, v := range content {
					m := v.(map[string]any)
					if m["type"] == "text/html" {
						c.HTML = str(m["value"])
					} else {
						c.Text = str(m["value"])
					}
				}
				return c, true
			},
			failNext: func(class error) {
				switch class {
				case email.ErrRateLimited:
					api.respond(429, `{"errors":[{"message":"too many requests","field":null}]}`)
				case email.ErrQuotaExceeded:
					api.respond(401, `{"errors":[{"message":"Maximum credits exceeded","field":null}]}`)
				case email.ErrInvalidRecipient:
					api.respond(400, `{"errors":[{"message":"Does not contain a valid address.","field":"personalizations.0.to.0.email"}]}`)
				}
			},
		}
	})
}

func TestBrevoRequestShape(t *testing.T) {
	api, p := newFakeBrevoAPI(t)
	msg := contractMessage()
	msg.To = []email.Address{{Address: "user@example.com"}, {Name: "Nguyễn, A", Address: "a@example.com"}}
	msg.ReplyTo = email.Address{Name: "Hỗ trợ", Address: "support@hoanxu.com"}
	msg.Headers = map[string]string{"List-Unsubscribe": "<https://hoanxu.com/u>"}
	msg.Tags = map[string]string{"server": "hoantien.work"}
	msg.IdempotencyKey = "cashback_paid:abc"
	result, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if result != (email.Result{Provider: "brevo", ProviderMessageID: "<202611210800.123@smtp-relay.mailin.fr>"}) {
		t.Fatalf("result %+v", result)
	}
	req, body, _ := api.last()
	if req.Method != http.MethodPost || req.URL.Path != "/v3/smtp/email" || req.Header.Get("api-key") != "xkeysib-test" {
		t.Fatalf("%s %s key %q", req.Method, req.URL.Path, req.Header.Get("api-key"))
	}
	want := map[string]any{
		"sender":      map[string]any{"name": "Hoàn Xu", "email": "noreply@hoanxu.com"},
		"to":          []any{map[string]any{"email": "user@example.com"}, map[string]any{"email": "a@example.com", "name": "Nguyễn, A"}},
		"replyTo":     map[string]any{"email": "support@hoanxu.com", "name": "Hỗ trợ"},
		"subject":     msg.Subject,
		"htmlContent": msg.HTML,
		"textContent": msg.Text,
		"headers":     map[string]any{"List-Unsubscribe": "<https://hoanxu.com/u>", "idempotencyKey": "cashback_paid:abc"},
		"tags":        []any{"cashback_paid", "server:hoantien_work"},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body\n%#v\nwant\n%#v", body, want)
	}
	// The caller's header map is not modified.
	if _, leaked := msg.Headers["idempotencyKey"]; leaked {
		t.Fatal("idempotency key written into the message headers")
	}
	p.cfg.SkipIdempotencyKey = true
	msg.Headers = nil
	if _, err := p.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := api.last(); body["headers"] != nil {
		t.Fatalf("headers %v", body["headers"])
	}
}

func TestSendGridRequestShape(t *testing.T) {
	api, p := newFakeSendGridAPI(t)
	msg := contractMessage()
	msg.To = []email.Address{{Address: "user@example.com"}, {Name: "Nguyễn, A", Address: "a@example.com"}}
	msg.ReplyTo = email.Address{Address: "support@hoanxu.com"}
	msg.Headers = map[string]string{"List-Unsubscribe": "<https://hoanxu.com/u>"}
	msg.Tags = map[string]string{"server": "hoantien.work"}
	msg.IdempotencyKey = "cashback_paid:abc"
	result, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if result != (email.Result{Provider: "sendgrid", ProviderMessageID: "sg_123"}) {
		t.Fatalf("result %+v", result)
	}
	req, body, _ := api.last()
	if req.Method != http.MethodPost || req.URL.Path != "/v3/mail/send" || req.Header.Get("Authorization") != "Bearer SG.test" {
		t.Fatalf("%s %s auth %q", req.Method, req.URL.Path, req.Header.Get("Authorization"))
	}
	want := map[string]any{
		"personalizations": []any{map[string]any{"to": []any{map[string]any{"email": "user@example.com"}, map[string]any{"email": "a@example.com", "name": "Nguyễn, A"}}}},
		"from":             map[string]any{"email": "noreply@hoanxu.com", "name": "Hoàn Xu"},
		"reply_to":         map[string]any{"email": "support@hoanxu.com"},
		"subject":          msg.Subject,
		"content":          []any{map[string]any{"type": "text/plain", "value": msg.Text}, map[string]any{"type": "text/html", "value": msg.HTML}},
		"headers":          map[string]any{"List-Unsubscribe": "<https://hoanxu.com/u>"},
		"categories":       []any{"cashback_paid"},
		"custom_args":      map[string]any{"server": "hoantien.work", "idempotency_key": "cashback_paid:abc"},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body\n%#v\nwant\n%#v", body, want)
	}
}

func TestBrevoErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status        int
		code, message string
		want          error
	}{
		{429, "", "Too many requests", email.ErrRateLimited},
		{402, "not_enough_credits", "not enough credits", email.ErrQuotaExceeded},
		{403, "account_under_validation", "Your account is under validation", email.ErrQuotaExceeded},
		{500, "", "", email.ErrTemporary},
		{400, "invalid_parameter", "email is not valid in to", email.ErrInvalidRecipient},
		{400, "invalid_parameter", "sender email is not valid", email.ErrPermanent},
		{400, "invalid_parameter", "Sending has been rejected because the sender you used noreply@hoanxu.com is not valid.", email.ErrPermanent},
		{401, "unauthorized", "Key not found", email.ErrPermanent},
	} {
		if got := classifyBrevo(tc.status, tc.code, tc.message); got != tc.want {
			t.Errorf("%d %s %q: %s, want %s", tc.status, tc.code, tc.message, email.ClassName(got), email.ClassName(tc.want))
		}
	}
}

func TestSendGridErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status          int
		message, fields string
		want            error
	}{
		{429, "too many requests", "", email.ErrRateLimited},
		{401, "Maximum credits exceeded", "", email.ErrQuotaExceeded},
		{502, "", "", email.ErrTemporary},
		{400, "Does not contain a valid address.", "personalizations.0.to.0.email", email.ErrInvalidRecipient},
		{403, "The from address does not match a verified Sender Identity.", "from", email.ErrPermanent},
		{401, "The provided authorization grant is invalid, expired, or revoked", "", email.ErrPermanent},
	} {
		if got := classifySendGrid(tc.status, tc.message, tc.fields); got != tc.want {
			t.Errorf("%d %q: %s, want %s", tc.status, tc.message, email.ClassName(got), email.ClassName(tc.want))
		}
	}
}

func TestBrevoAndSendGridNetworkErrorsAreTemporaryAndKeysNeverInErrors(t *testing.T) {
	brevo, _ := NewBrevo(BrevoConfig{APIKey: "xkeysib-secret", BaseURL: "http://127.0.0.1:1"}, nil)
	sendgrid, _ := NewSendGrid(SendGridConfig{APIKey: "SG.secret", BaseURL: "http://127.0.0.1:1"}, nil)
	for _, p := range []email.Sender{brevo, sendgrid} {
		_, err := p.Send(context.Background(), contractMessage())
		if !errors.Is(err, email.ErrTemporary) {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("API key leaked into the error")
		}
	}
}
