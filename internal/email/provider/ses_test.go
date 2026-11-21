package provider

import (
	"context"
	"errors"
	"testing"

	"mail-router/internal/email"

	"github.com/aws/smithy-go"
)

func TestSESRequestShape(t *testing.T) {
	client := &fakeSESClient{}
	p, _ := NewSES(client, SESConfig{Region: "ap-southeast-1", ConfigurationSet: "hoanxu-transactional"})
	msg := contractMessage()
	msg.ReplyTo = email.Address{Address: "support@hoanxu.com"}
	msg.Headers = map[string]string{"List-Unsubscribe": "<https://hoanxu.com/u>"}
	msg.Tags = map[string]string{"server": "hoanxu.com"}
	result, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if result != (email.Result{Provider: "ses", ProviderMessageID: "ses-1"}) {
		t.Fatalf("result %+v", result)
	}
	in, _ := client.last()
	if got := *in.FromEmailAddress; got != "=?utf-8?q?Ho=C3=A0n_Xu?= <noreply@hoanxu.com>" {
		t.Errorf("from %q", got)
	}
	if in.Destination.ToAddresses[0] != "user@example.com" || in.ReplyToAddresses[0] != "support@hoanxu.com" {
		t.Errorf("addresses %+v %+v", in.Destination, in.ReplyToAddresses)
	}
	simple := in.Content.Simple
	if *simple.Subject.Data != msg.Subject || *simple.Subject.Charset != "UTF-8" ||
		*simple.Body.Html.Data != msg.HTML || *simple.Body.Text.Data != msg.Text {
		t.Errorf("content %+v", simple)
	}
	if len(simple.Headers) != 1 || *simple.Headers[0].Name != "List-Unsubscribe" {
		t.Errorf("headers %+v", simple.Headers)
	}
	if *in.ConfigurationSetName != "hoanxu-transactional" {
		t.Errorf("configuration set %v", in.ConfigurationSetName)
	}
	if len(in.EmailTags) != 2 || *in.EmailTags[0].Value != "cashback_paid" || *in.EmailTags[1].Value != "hoanxu_com" {
		t.Errorf("tags %+v", in.EmailTags)
	}
}

func TestSESErrorMapping(t *testing.T) {
	for code, want := range map[string]error{
		"TooManyRequestsException":           email.ErrRateLimited,
		"LimitExceededException":             email.ErrQuotaExceeded,
		"SendingPausedException":             email.ErrQuotaExceeded,
		"AccountSuspendedException":          email.ErrPermanent,
		"MailFromDomainNotVerifiedException": email.ErrPermanent,
		"BadRequestException":                email.ErrPermanent,
		"InternalFailure":                    email.ErrTemporary,
	} {
		if got := classifySES(&smithy.GenericAPIError{Code: code}); got != want {
			t.Errorf("%s: %s, want %s", code, email.ClassName(got), email.ClassName(want))
		}
	}
	if classifySES(&smithy.GenericAPIError{Code: "MessageRejected", Message: "Email address is not verified."}) != email.ErrInvalidRecipient {
		t.Error("unverified recipient")
	}
	if classifySES(errors.New("dial tcp: i/o timeout")) != email.ErrTemporary {
		t.Error("network error")
	}
}
