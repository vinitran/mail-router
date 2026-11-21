package provider

import (
	"strings"
	"testing"

	"mail-router/internal/email"
)

func env(values map[string]string) email.Env {
	return func(key string) string { return values[key] }
}

func TestEveryProviderIsRegistered(t *testing.T) {
	got := strings.Join(email.Providers(), ",")
	for _, name := range []string{"brevo", "log", "noop", "resend", "sendgrid", "ses", "smtp"} {
		if !strings.Contains(got, name) {
			t.Errorf("%s not registered (%s)", name, got)
		}
	}
}

func TestFactoriesValidateConfiguration(t *testing.T) {
	isolateAWS(t)
	for _, tc := range []struct {
		provider string
		env      map[string]string
		wantErr  string
	}{
		{"smtp", map[string]string{}, "EMAIL_SMTP_PASSWORD, EMAIL_SMTP_USERNAME required"},
		{"smtp", map[string]string{"EMAIL_SMTP_USERNAME": "u", "EMAIL_SMTP_PASSWORD": "p", "EMAIL_SMTP_PORT": "70000"}, "not a valid port"},
		{"smtp", map[string]string{"EMAIL_SMTP_USERNAME": "u", "EMAIL_SMTP_PASSWORD": "p", "EMAIL_SMTP_TEXT_PART": "maybe"}, "EMAIL_SMTP_TEXT_PART must be true or false"},
		{"smtp", map[string]string{"EMAIL_SMTP_USERNAME": "u", "EMAIL_SMTP_PASSWORD": "p"}, ""},
		{"resend", map[string]string{}, "RESEND_API_KEY required"},
		{"resend", map[string]string{"RESEND_API_KEY": "k", "RESEND_TIMEOUT": "soon"}, "RESEND_TIMEOUT must be a positive duration"},
		{"resend", map[string]string{"RESEND_API_KEY": "k"}, ""},
		{"brevo", map[string]string{}, "BREVO_API_KEY required"},
		{"brevo", map[string]string{"BREVO_API_KEY": "k", "BREVO_IDEMPOTENCY": "maybe"}, "BREVO_IDEMPOTENCY must be true or false"},
		{"brevo", map[string]string{"BREVO_API_KEY": "k", "BREVO_API_URL": "api.brevo.com"}, "BREVO_API_URL must be an http(s) URL"},
		{"brevo", map[string]string{"BREVO_API_KEY": "k"}, ""},
		{"sendgrid", map[string]string{}, "SENDGRID_API_KEY required"},
		{"sendgrid", map[string]string{"SENDGRID_API_KEY": "k", "SENDGRID_TIMEOUT": "0s"}, "SENDGRID_TIMEOUT must be a positive duration"},
		{"sendgrid", map[string]string{"SENDGRID_API_KEY": "k", "SENDGRID_API_URL": "https://api.eu.sendgrid.com"}, ""},
		{"ses", map[string]string{}, "AWS_REGION (or SES_REGION) required"},
		{"ses", map[string]string{"AWS_REGION": "ap-southeast-1"}, ""},
		{"ses", map[string]string{"SES_REGION": "ap-southeast-1", "SES_CONFIGURATION_SET": "cs"}, ""},
		{"log", nil, ""},
		{"mailgun", nil, `email provider "mailgun" is not registered (available: brevo, log, noop, resend, sendgrid, ses, smtp)`},
	} {
		sender, err := email.NewProvider(tc.provider, env(tc.env))
		switch {
		case tc.wantErr == "" && (err != nil || sender == nil):
			t.Errorf("%s %v: unexpected error %v", tc.provider, tc.env, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s %v: error %v, want %q", tc.provider, tc.env, err, tc.wantErr)
		}
	}
}

func TestSESFactoryUsesRegion(t *testing.T) {
	isolateAWS(t)
	sender, err := email.NewProvider("ses", env(map[string]string{"AWS_REGION": "ap-southeast-1", "SES_CONFIGURATION_SET": "cs"}))
	if err != nil {
		t.Fatal(err)
	}
	ses := sender.(*SES)
	if ses.cfg.Region != "ap-southeast-1" || ses.cfg.ConfigurationSet != "cs" {
		t.Fatalf("config %+v", ses.cfg)
	}
}

// isolateAWS keeps the developer's AWS files and variables out of the
// factory tests (nothing here contacts AWS).
func isolateAWS(t *testing.T) {
	for _, key := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(key, "")
	}
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func TestMaskAddress(t *testing.T) {
	if MaskAddress("vinh@example.com") != "v***@example.com" || MaskAddress("bad") != "***" {
		t.Fatal("mask")
	}
}
