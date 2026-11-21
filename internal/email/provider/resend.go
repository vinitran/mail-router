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

// ResendName selects the Resend HTTP API (EMAIL_PROVIDER=resend).
const ResendName = "resend"

func init() { email.Register(ResendName, newResendFromEnv) }

// ResendConfig is read from RESEND_*.
type ResendConfig struct {
	APIKey  string        // RESEND_API_KEY, required
	BaseURL string        // RESEND_API_URL, default https://api.resend.com
	Timeout time.Duration // RESEND_TIMEOUT, default 15s
	// SkipIdempotencyKey stops sending Message.IdempotencyKey as the
	// Idempotency-Key header (RESEND_IDEMPOTENCY=false).
	SkipIdempotencyKey bool
}

func newResendFromEnv(env email.Env) (email.Sender, error) {
	timeout, err := env.Duration("RESEND_TIMEOUT", 15*time.Second)
	if err != nil {
		return nil, err
	}
	idempotency, err := env.Bool("RESEND_IDEMPOTENCY", true)
	if err != nil {
		return nil, err
	}
	return NewResend(ResendConfig{
		APIKey:             env.String("RESEND_API_KEY", ""),
		BaseURL:            env.String("RESEND_API_URL", "https://api.resend.com"),
		Timeout:            timeout,
		SkipIdempotencyKey: !idempotency,
	}, nil)
}

// NewResend validates cfg. client may be nil.
func NewResend(cfg ResendConfig, client *http.Client) (*Resend, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("RESEND_API_KEY required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.resend.com"
	}
	if !strings.HasPrefix(cfg.BaseURL, "https://") && !strings.HasPrefix(cfg.BaseURL, "http://") {
		return nil, fmt.Errorf("RESEND_API_URL must be an http(s) URL")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &Resend{cfg: cfg, client: client}, nil
}

// Resend sends through POST /emails of the Resend API.
type Resend struct {
	cfg    ResendConfig
	client *http.Client
}

type resendTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resendRequest struct {
	From    string            `json:"from"`
	To      []string          `json:"to"`
	Subject string            `json:"subject"`
	HTML    string            `json:"html,omitempty"`
	Text    string            `json:"text,omitempty"`
	ReplyTo []string          `json:"reply_to,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Tags    []resendTag       `json:"tags,omitempty"`
}

type resendResponse struct {
	ID         string `json:"id"`
	StatusCode int    `json:"statusCode"`
	Name       string `json:"name"`
	Message    string `json:"message"`
}

// Send implements email.Sender.
func (r *Resend) Send(ctx context.Context, msg email.Message) (email.Result, error) {
	result := email.Result{Provider: ResendName}
	payload := resendRequest{
		From:    displayAddress(msg.From),
		Subject: msg.Subject,
		HTML:    msg.HTML,
		Text:    msg.Text,
		Headers: msg.Headers,
		Tags:    resendTags(msg),
	}
	for _, to := range msg.To {
		payload.To = append(payload.To, displayAddress(to))
	}
	if !msg.ReplyTo.IsZero() {
		payload.ReplyTo = []string{displayAddress(msg.ReplyTo)}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return result, email.Classify(ResendName, email.ErrPermanent, fmt.Errorf("encode resend request: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.cfg.BaseURL, "/")+"/emails", bytes.NewReader(body))
	if err != nil {
		return result, email.Classify(ResendName, email.ErrPermanent, fmt.Errorf("build resend request: %w", err))
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "hoanxu-mail-router")
	if msg.IdempotencyKey != "" && !r.cfg.SkipIdempotencyKey {
		req.Header.Set("Idempotency-Key", msg.IdempotencyKey)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return result, email.Classify(ResendName, email.ErrTemporary, fmt.Errorf("resend request: %w", err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var decoded resendResponse
	_ = json.Unmarshal(raw, &decoded)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.ProviderMessageID = decoded.ID
		return result, nil
	}
	// The API message never echoes the API key; it can name the recipient,
	// so callers keep not logging raw provider errors.
	err = fmt.Errorf("resend: HTTP %d %s: %s", resp.StatusCode, decoded.Name, decoded.Message)
	return result, email.Classify(ResendName, classifyResend(resp.StatusCode, decoded.Name, decoded.Message), err)
}

func classifyResend(status int, name, message string) error {
	name, message = strings.ToLower(name), strings.ToLower(message)
	switch {
	case status == http.StatusTooManyRequests && strings.Contains(name, "quota"):
		return email.ErrQuotaExceeded // daily_quota_exceeded, monthly_quota_exceeded
	case status == http.StatusTooManyRequests:
		return email.ErrRateLimited // rate_limit_exceeded
	case status >= 500, name == "concurrent_idempotent_requests":
		return email.ErrTemporary
	case name == "invalid_to_address", strings.Contains(message, "`to`"), strings.Contains(message, "to field"), strings.Contains(message, "recipient"):
		return email.ErrInvalidRecipient
	default:
		return email.ErrPermanent // validation, auth, unverified domain, ...
	}
}

// resendTags turns Category and Tags into Resend tags (ASCII letters,
// digits, "_" and "-" only).
func resendTags(msg email.Message) []resendTag {
	var tags []resendTag
	if msg.Category != "" {
		tags = append(tags, resendTag{Name: "category", Value: tagValue(msg.Category)})
	}
	keys := make([]string, 0, len(msg.Tags))
	for key := range msg.Tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		tags = append(tags, resendTag{Name: tagValue(key), Value: tagValue(msg.Tags[key])})
	}
	return tags
}

func tagValue(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			b[i] = '_'
		}
	}
	if len(b) > 256 {
		b = b[:256]
	}
	return string(b)
}

// displayAddress is "Name <address>" (UTF-8 names allowed by HTTP APIs),
// quoting names that contain address specials, or the bare address.
func displayAddress(a email.Address) string {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return a.Address
	}
	if strings.ContainsAny(name, "\"(),:;<>@[\\]") {
		name = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
	}
	return name + " <" + a.Address + ">"
}
