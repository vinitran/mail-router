package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mail-router/internal/email"
)

// SendGridName selects the Twilio SendGrid v3 Mail Send API
// (EMAIL_PROVIDER=sendgrid).
const SendGridName = "sendgrid"

func init() { email.Register(SendGridName, newSendGridFromEnv) }

// SendGridConfig is read from SENDGRID_*.
type SendGridConfig struct {
	APIKey string // SENDGRID_API_KEY (SG....), required
	// BaseURL is SENDGRID_API_URL, default https://api.sendgrid.com
	// (https://api.eu.sendgrid.com for an EU-pinned subuser).
	BaseURL string
	Timeout time.Duration // SENDGRID_TIMEOUT, default 15s
}

func newSendGridFromEnv(env email.Env) (email.Sender, error) {
	timeout, err := env.Duration("SENDGRID_TIMEOUT", 15*time.Second)
	if err != nil {
		return nil, err
	}
	return NewSendGrid(SendGridConfig{
		APIKey:  env.String("SENDGRID_API_KEY", ""),
		BaseURL: env.String("SENDGRID_API_URL", "https://api.sendgrid.com"),
		Timeout: timeout,
	}, nil)
}

// NewSendGrid validates cfg. client may be nil.
func NewSendGrid(cfg SendGridConfig, client *http.Client) (*SendGrid, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("SENDGRID_API_KEY required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.sendgrid.com"
	}
	if !strings.HasPrefix(cfg.BaseURL, "https://") && !strings.HasPrefix(cfg.BaseURL, "http://") {
		return nil, fmt.Errorf("SENDGRID_API_URL must be an http(s) URL")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &SendGrid{cfg: cfg, client: client}, nil
}

// SendGrid sends through POST /v3/mail/send. SendGrid has no idempotency
// key; Message.IdempotencyKey is sent as a custom arg so a duplicate can at
// least be found in the activity feed.
type SendGrid struct {
	cfg    SendGridConfig
	client *http.Client
}

type sendGridAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type sendGridContent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type sendGridPersonalization struct {
	To []sendGridAddress `json:"to"`
}

type sendGridRequest struct {
	Personalizations []sendGridPersonalization `json:"personalizations"`
	From             sendGridAddress           `json:"from"`
	ReplyTo          *sendGridAddress          `json:"reply_to,omitempty"`
	Subject          string                    `json:"subject"`
	Content          []sendGridContent         `json:"content"`
	Headers          map[string]string         `json:"headers,omitempty"`
	Categories       []string                  `json:"categories,omitempty"`
	CustomArgs       map[string]string         `json:"custom_args,omitempty"`
}

type sendGridResponse struct {
	Errors []struct {
		Message string `json:"message"`
		Field   string `json:"field"`
	} `json:"errors"`
}

// Send implements email.Sender.
func (s *SendGrid) Send(ctx context.Context, msg email.Message) (email.Result, error) {
	result := email.Result{Provider: SendGridName}
	payload := sendGridRequest{
		From:    sendGridAddress{Email: msg.From.Address, Name: strings.TrimSpace(msg.From.Name)},
		Subject: msg.Subject,
		Headers: msg.Headers,
	}
	var to []sendGridAddress
	for _, addr := range msg.To {
		to = append(to, sendGridAddress{Email: addr.Address, Name: strings.TrimSpace(addr.Name)})
	}
	payload.Personalizations = []sendGridPersonalization{{To: to}}
	if !msg.ReplyTo.IsZero() {
		payload.ReplyTo = &sendGridAddress{Email: msg.ReplyTo.Address, Name: strings.TrimSpace(msg.ReplyTo.Name)}
	}
	// SendGrid requires text/plain before text/html and rejects empty values.
	if msg.Text != "" {
		payload.Content = append(payload.Content, sendGridContent{Type: "text/plain", Value: msg.Text})
	}
	if msg.HTML != "" {
		payload.Content = append(payload.Content, sendGridContent{Type: "text/html", Value: msg.HTML})
	}
	if msg.Category != "" {
		payload.Categories = []string{tagValue(msg.Category)}
	}
	if len(msg.Tags) > 0 || msg.IdempotencyKey != "" {
		payload.CustomArgs = make(map[string]string, len(msg.Tags)+1)
		for k, v := range msg.Tags {
			payload.CustomArgs[k] = v
		}
		if msg.IdempotencyKey != "" {
			payload.CustomArgs["idempotency_key"] = msg.IdempotencyKey
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return result, email.Classify(SendGridName, email.ErrPermanent, fmt.Errorf("encode sendgrid request: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.BaseURL, "/")+"/v3/mail/send", bytes.NewReader(body))
	if err != nil {
		return result, email.Classify(SendGridName, email.ErrPermanent, fmt.Errorf("build sendgrid request: %w", err))
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hoanxu-mail-router")
	resp, err := s.client.Do(req)
	if err != nil {
		return result, email.Classify(SendGridName, email.ErrTemporary, fmt.Errorf("sendgrid request: %w", err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.ProviderMessageID = resp.Header.Get("X-Message-Id")
		return result, nil
	}
	var decoded sendGridResponse
	_ = json.Unmarshal(raw, &decoded)
	var messages, fields []string
	for _, e := range decoded.Errors {
		messages = append(messages, e.Message)
		fields = append(fields, e.Field)
	}
	// SendGrid messages never echo the API key; they can name the recipient,
	// so callers keep not logging raw provider errors.
	message := strings.Join(messages, "; ")
	err = fmt.Errorf("sendgrid: HTTP %d: %s", resp.StatusCode, message)
	return result, email.Classify(SendGridName, classifySendGrid(resp.StatusCode, message, strings.Join(fields, " ")), err)
}

func classifySendGrid(status int, message, fields string) error {
	message, fields = strings.ToLower(message), strings.ToLower(fields)
	switch {
	case status == http.StatusTooManyRequests:
		return email.ErrRateLimited
	case strings.Contains(message, "credits"), strings.Contains(message, "quota"):
		// "Maximum credits exceeded" (401): the plan's sends are used up.
		return email.ErrQuotaExceeded
	case status >= 500:
		return email.ErrTemporary
	case strings.Contains(fields, "personalizations") && strings.Contains(fields, ".to"):
		return email.ErrInvalidRecipient
	default:
		return email.ErrPermanent // auth, unverified sender identity, invalid request, ...
	}
}
