// Package server is the mail-router HTTP API.
//
//	POST /v1/send   send one message (Bearer token)
//	GET  /v1/stats  send counts by provider and outcome (Bearer token)
//	GET  /healthz   liveness, no token
//
// See README.md for the request and response format.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"mail-router/internal/email"
)

// MaxBodyBytes bounds a /v1/send request (subject, HTML and text included).
const MaxBodyBytes = 2 << 20

// Config configures the API.
type Config struct {
	Token       string        // MAIL_ROUTER_TOKEN, required
	SendTimeout time.Duration // MAIL_ROUTER_SEND_TIMEOUT: one message through every provider
	DefaultFrom email.Address // EMAIL_FROM_NAME / EMAIL_FROM_ADDRESS
	Delivery    *email.Delivery
	Idempotency *Idempotency
	Logf        func(format string, args ...any)
}

// Server serves the API.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// New returns the API handler.
func New(cfg Config) *Server {
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = 60 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.DefaultFrom.Name == "" {
		cfg.DefaultFrom.Name = email.DefaultFromName
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("POST /v1/send", s.auth(s.send))
	s.mux.HandleFunc("GET /v1/stats", s.auth(s.stats))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Wire format.

type address struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// SendRequest is the body of POST /v1/send.
type SendRequest struct {
	From           *address          `json:"from,omitempty"`
	To             []address         `json:"to"`
	ReplyTo        *address          `json:"reply_to,omitempty"`
	Subject        string            `json:"subject"`
	HTML           string            `json:"html,omitempty"`
	Text           string            `json:"text,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Category       string            `json:"category,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

// SendResponse is the body of a successful POST /v1/send.
type SendResponse struct {
	Provider          string `json:"provider"`
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	// Duplicate is true when the idempotency key was already sent; the
	// message was not sent again and the first result is returned.
	Duplicate bool `json:"duplicate,omitempty"`
}

// ErrorResponse is the body of every failed request.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody describes a failure. Class is one of rate_limited,
// quota_exceeded, invalid_recipient, temporary, permanent (or
// unauthorized for a bad token); callers retry on rate_limited, temporary
// and quota_exceeded only.
type ErrorBody struct {
	Class    string `json:"class"`
	Provider string `json:"provider,omitempty"`
	Message  string `json:"message"`
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) stats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"routing":   s.cfg.Delivery.Routing,
		"providers": s.cfg.Delivery.Providers,
		"counts":    s.cfg.Delivery.Counters.Snapshot(),
	})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + s.cfg.Token)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if s.cfg.Token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			writeJSON(w, http.StatusUnauthorized, ErrorResponse{ErrorBody{Class: "unauthorized", Message: "missing or wrong bearer token"}})
			return
		}
		next(w, r)
	}
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var req SendRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{ErrorBody{Class: "permanent", Message: "invalid JSON body: " + err.Error()}})
		return
	}
	if dec.More() {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{ErrorBody{Class: "permanent", Message: "invalid JSON body: trailing data"}})
		return
	}
	msg := s.message(req)
	if err := email.Validate(msg); err != nil {
		writeError(w, err)
		return
	}

	key := strings.TrimSpace(msg.IdempotencyKey)
	if key != "" && s.cfg.Idempotency != nil {
		claim, cached, ok := s.cfg.Idempotency.Claim(key)
		switch {
		case cached != nil:
			writeJSON(w, http.StatusOK, SendResponse{Provider: cached.Provider, ProviderMessageID: cached.ProviderMessageID, Duplicate: true})
			return
		case !ok:
			writeJSON(w, http.StatusConflict, ErrorResponse{ErrorBody{Class: "temporary", Message: "a message with this idempotency key is being sent; retry later"}})
			return
		}
		defer claim.Release()
		result, err := s.deliver(r.Context(), msg)
		if err != nil {
			writeError(w, err)
			return
		}
		claim.Done(result)
		writeJSON(w, http.StatusOK, SendResponse{Provider: result.Provider, ProviderMessageID: result.ProviderMessageID})
		return
	}

	result, err := s.deliver(r.Context(), msg)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, SendResponse{Provider: result.Provider, ProviderMessageID: result.ProviderMessageID})
}

// deliver sends msg. A caller that disconnects does not abort a send that
// already started: it finishes (bounded by SendTimeout) so the idempotency
// cache can answer the caller's retry.
func (s *Server) deliver(ctx context.Context, msg email.Message) (email.Result, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.SendTimeout)
	defer cancel()
	return s.cfg.Delivery.Sender.Send(ctx, msg)
}

func (s *Server) message(req SendRequest) email.Message {
	msg := email.Message{
		Subject:        req.Subject,
		HTML:           req.HTML,
		Text:           req.Text,
		Headers:        req.Headers,
		Category:       req.Category,
		Tags:           req.Tags,
		IdempotencyKey: req.IdempotencyKey,
	}
	if req.From != nil {
		msg.From = email.Address{Name: req.From.Name, Address: strings.TrimSpace(req.From.Address)}
	}
	if msg.From.Address == "" {
		msg.From.Address = s.cfg.DefaultFrom.Address
	}
	if strings.TrimSpace(msg.From.Name) == "" {
		msg.From.Name = s.cfg.DefaultFrom.Name
	}
	for _, to := range req.To {
		msg.To = append(msg.To, email.Address{Name: to.Name, Address: strings.TrimSpace(to.Address)})
	}
	if req.ReplyTo != nil {
		msg.ReplyTo = email.Address{Name: req.ReplyTo.Name, Address: strings.TrimSpace(req.ReplyTo.Address)}
	}
	return msg
}

func writeError(w http.ResponseWriter, err error) {
	class := email.ClassOf(err)
	body := ErrorBody{Class: email.ClassName(err), Message: err.Error()}
	if class == nil {
		body.Class = "permanent"
	}
	var classified *email.Error
	if errors.As(err, &classified) {
		body.Provider = classified.Provider
	}
	status := http.StatusBadGateway
	switch class {
	case email.ErrRateLimited, email.ErrQuotaExceeded:
		status = http.StatusTooManyRequests
	case email.ErrTemporary:
		status = http.StatusServiceUnavailable
	case email.ErrInvalidRecipient:
		status = http.StatusUnprocessableEntity
	case email.ErrPermanent:
		if body.Provider == "" {
			status = http.StatusBadRequest // rejected by Validate, never sent
		}
	}
	writeJSON(w, status, ErrorResponse{body})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
