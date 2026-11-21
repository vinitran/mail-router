package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"mail-router/internal/email"
)

// BrevoName selects the Brevo (formerly Sendinblue) transactional email API
// (EMAIL_PROVIDER=brevo).
const BrevoName = "brevo"

func init() { email.Register(BrevoName, newBrevoFromEnv) }

// BrevoConfig is read from BREVO_*.
type BrevoConfig struct {
	APIKey  string        // BREVO_API_KEY (xkeysib-...), required
	BaseURL string        // BREVO_API_URL, default https://api.brevo.com
	Timeout time.Duration // BREVO_TIMEOUT, default 15s
	// SkipIdempotencyKey stops sending Message.IdempotencyKey as Brevo's
	// idempotencyKey header (BREVO_IDEMPOTENCY=false).
	SkipIdempotencyKey bool
}

func newBrevoFromEnv(env email.Env) (email.Sender, error) {
	timeout, err := env.Duration("BREVO_TIMEOUT", 15*time.Second)
	if err != nil {
		return nil, err
	}
	idempotency, err := env.Bool("BREVO_IDEMPOTENCY", true)
	if err != nil {
		return nil, err
	}
	return NewBrevo(BrevoConfig{
		APIKey:             env.String("BREVO_API_KEY", ""),
		BaseURL:            env.String("BREVO_API_URL", "https://api.brevo.com"),
		Timeout:            timeout,
		SkipIdempotencyKey: !idempotency,
	}, nil)
}

// NewBrevo validates cfg. client may be nil.
func NewBrevo(cfg BrevoConfig, client *http.Client) (*Brevo, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("BREVO_API_KEY required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.brevo.com"
	}
	if !strings.HasPrefix(cfg.BaseURL, "https://") && !strings.HasPrefix(cfg.BaseURL, "http://") {
		return nil, fmt.Errorf("BREVO_API_URL must be an http(s) URL")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &Brevo{cfg: cfg, client: client}, nil
}

// Brevo sends through POST /v3/smtp/email of the Brevo API.
type Brevo struct {
	cfg    BrevoConfig
	client *http.Client
}

type brevoAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type brevoRequest struct {
	Sender      brevoAddress      `json:"sender"`
	To          []brevoAddress    `json:"to"`
	ReplyTo     *brevoAddress     `json:"replyTo,omitempty"`
	Subject     string            `json:"subject"`
	HTMLContent string            `json:"htmlContent,omitempty"`
	TextContent string            `json:"textContent,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
}

type brevoResponse struct {
	MessageID string `json:"messageId"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// Send implements email.Sender.
func (b *Brevo) Send(ctx context.Context, msg email.Message) (email.Result, error) {
	result := email.Result{Provider: BrevoName}
	payload := brevoRequest{
		Sender:      brevoAddress{Email: msg.From.Address, Name: strings.TrimSpace(msg.From.Name)},
		Subject:     msg.Subject,
		HTMLContent: msg.HTML,
		TextContent: msg.Text,
		Tags:        brevoTags(msg),
	}
	for _, to := range msg.To {
		payload.To = append(payload.To, brevoAddress{Email: to.Address, Name: strings.TrimSpace(to.Name)})
	}
	if !msg.ReplyTo.IsZero() {
		payload.ReplyTo = &brevoAddress{Email: msg.ReplyTo.Address, Name: strings.TrimSpace(msg.ReplyTo.Name)}
	}
	if len(msg.Headers) > 0 || (msg.IdempotencyKey != "" && !b.cfg.SkipIdempotencyKey) {
		payload.Headers = make(map[string]string, len(msg.Headers)+1)
		for k, v := range msg.Headers {
			payload.Headers[k] = v
		}
		if msg.IdempotencyKey != "" && !b.cfg.SkipIdempotencyKey {
			payload.Headers["idempotencyKey"] = msg.IdempotencyKey
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return result, email.Classify(BrevoName, email.ErrPermanent, fmt.Errorf("encode brevo request: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.cfg.BaseURL, "/")+"/v3/smtp/email", bytes.NewReader(body))
	if err != nil {
		return result, email.Classify(BrevoName, email.ErrPermanent, fmt.Errorf("build brevo request: %w", err))
	}
	req.Header.Set("api-key", b.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "hoanxu-mail-router")
	resp, err := b.client.Do(req)
	if err != nil {
		return result, email.Classify(BrevoName, email.ErrTemporary, fmt.Errorf("brevo request: %w", err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var decoded brevoResponse
	_ = json.Unmarshal(raw, &decoded)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.ProviderMessageID = decoded.MessageID
		return result, nil
	}
	// Brevo messages never echo the API key; they can name the recipient,
	// so callers keep not logging raw provider errors.
	err = fmt.Errorf("brevo: HTTP %d %s: %s", resp.StatusCode, decoded.Code, decoded.Message)
	return result, email.Classify(BrevoName, classifyBrevo(resp.StatusCode, decoded.Code, decoded.Message), err)
}

func classifyBrevo(status int, code, message string) error {
	code, message = strings.ToLower(code), strings.ToLower(message)
	switch {
	case status == http.StatusTooManyRequests:
		return email.ErrRateLimited
	case status == http.StatusPaymentRequired, code == "not_enough_credits", code == "account_under_validation",
		strings.Contains(message, "credits"), strings.Contains(message, "quota"), strings.Contains(message, "suspended"):
		// Out of daily/plan credits, or sending is held while Brevo reviews
		// the account: this account will not send for a while.
		return email.ErrQuotaExceeded
	case status >= 500:
		return email.ErrTemporary
	case strings.Contains(message, "sender"):
		return email.ErrPermanent // sender not valid / not verified
	case strings.Contains(message, "email") && (strings.Contains(message, " to") || strings.Contains(message, "recipient")),
		strings.Contains(message, "blacklisted"), strings.Contains(message, "blocked contact"):
		return email.ErrInvalidRecipient
	default:
		return email.ErrPermanent // unauthorized, invalid parameters, ...
	}
}

// brevoTags turns Category and Tags into Brevo tags ("name" for Category,
// "key:value" for Tags).
func brevoTags(msg email.Message) []string {
	var tags []string
	if msg.Category != "" {
		tags = append(tags, tagValue(msg.Category))
	}
	keys := make([]string, 0, len(msg.Tags))
	for key := range msg.Tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		tags = append(tags, tagValue(key)+":"+tagValue(msg.Tags[key]))
	}
	return tags
}
