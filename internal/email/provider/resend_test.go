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

func TestResendRequestShape(t *testing.T) {
	api := newFakeResendAPI(t)
	msg := contractMessage()
	msg.To = []email.Address{{Address: "user@example.com"}, {Name: "Nguyễn, A", Address: "a@example.com"}}
	msg.ReplyTo = email.Address{Address: "support@hoanxu.com"}
	msg.Headers = map[string]string{"List-Unsubscribe": "<https://hoanxu.com/u>"}
	msg.Tags = map[string]string{"server": "hoantien.work"}
	msg.IdempotencyKey = "cashback_paid:abc"
	result, err := api.resend(t).Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if result != (email.Result{Provider: "resend", ProviderMessageID: "re_123"}) {
		t.Fatalf("result %+v", result)
	}
	req, body, _ := api.last()
	if req.Method != http.MethodPost || req.URL.Path != "/emails" {
		t.Fatalf("%s %s", req.Method, req.URL.Path)
	}
	for header, want := range map[string]string{
		"Authorization":   "Bearer re_test_key",
		"Content-Type":    "application/json",
		"Idempotency-Key": "cashback_paid:abc",
	} {
		if got := req.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	want := map[string]any{
		"from":     "Hoàn Xu <noreply@hoanxu.com>",
		"to":       []any{"user@example.com", `"Nguyễn, A" <a@example.com>`},
		"subject":  msg.Subject,
		"html":     msg.HTML,
		"text":     msg.Text,
		"reply_to": []any{"support@hoanxu.com"},
		"headers":  map[string]any{"List-Unsubscribe": "<https://hoanxu.com/u>"},
		"tags": []any{
			map[string]any{"name": "category", "value": "cashback_paid"},
			map[string]any{"name": "server", "value": "hoantien_work"},
		},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body\n%#v\nwant\n%#v", body, want)
	}
}

func TestResendIdempotencyCanBeDisabled(t *testing.T) {
	api := newFakeResendAPI(t)
	p := api.resend(t)
	p.cfg.SkipIdempotencyKey = true
	msg := contractMessage()
	msg.IdempotencyKey = "k"
	if _, err := p.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if req, _, _ := api.last(); req.Header.Get("Idempotency-Key") != "" {
		t.Fatal("idempotency key sent")
	}
}

func TestResendErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		name   string
		msg    string
		want   error
	}{
		{429, "rate_limit_exceeded", "Too many requests", email.ErrRateLimited},
		{429, "daily_quota_exceeded", "daily quota", email.ErrQuotaExceeded},
		{429, "monthly_quota_exceeded", "monthly quota", email.ErrQuotaExceeded},
		{500, "application_error", "boom", email.ErrTemporary},
		{503, "", "", email.ErrTemporary},
		{409, "concurrent_idempotent_requests", "", email.ErrTemporary},
		{422, "validation_error", "Invalid `to` field.", email.ErrInvalidRecipient},
		{403, "validation_error", "The hoanxu.com domain is not verified.", email.ErrPermanent},
		{401, "missing_api_key", "Missing API key", email.ErrPermanent},
	} {
		if got := classifyResend(tc.status, tc.name, tc.msg); got != tc.want {
			t.Errorf("%d %s: %s, want %s", tc.status, tc.name, email.ClassName(got), email.ClassName(tc.want))
		}
	}
}

func TestResendNetworkErrorIsTemporaryAndKeyNeverInError(t *testing.T) {
	p, _ := NewResend(ResendConfig{APIKey: "re_secret_value", BaseURL: "http://127.0.0.1:1"}, nil)
	_, err := p.Send(context.Background(), contractMessage())
	if !errors.Is(err, email.ErrTemporary) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "re_secret_value") {
		t.Fatal("API key leaked into the error")
	}
}
