package provider

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"

	"mail-router/internal/email"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
)

// SESName selects Amazon SES v2 (EMAIL_PROVIDER=ses).
const SESName = "ses"

func init() { email.Register(SESName, newSESFromEnv) }

// SESConfig is read from the standard AWS settings plus SES_*.
type SESConfig struct {
	// Region: SES_REGION, else AWS_REGION / AWS_DEFAULT_REGION / shared config.
	Region string
	// ConfigurationSet: SES_CONFIGURATION_SET, optional (event publishing
	// for bounces/complaints).
	ConfigurationSet string
}

// SESAPI is the part of the SES v2 client the provider uses (a fake in tests).
type SESAPI interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, opts ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

func newSESFromEnv(env email.Env) (email.Sender, error) {
	cfg := SESConfig{
		Region:           env.String("SES_REGION", env.String("AWS_REGION", env.String("AWS_DEFAULT_REGION", ""))),
		ConfigurationSet: env.String("SES_CONFIGURATION_SET", ""),
	}
	// Credentials come from the default chain (AWS_ACCESS_KEY_ID /
	// AWS_SECRET_ACCESS_KEY, shared files, instance role). Nothing is
	// called here; a credential problem surfaces on the first send.
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if awsCfg.Region == "" {
		return nil, errors.New("AWS_REGION (or SES_REGION) required, e.g. ap-southeast-1")
	}
	cfg.Region = awsCfg.Region
	return NewSES(sesv2.NewFromConfig(awsCfg), cfg)
}

// NewSES wraps an SES v2 client.
func NewSES(client SESAPI, cfg SESConfig) (*SES, error) {
	if client == nil {
		return nil, errors.New("SES client required")
	}
	return &SES{client: client, cfg: cfg}, nil
}

// SES sends with SES v2 SendEmail and Simple content.
type SES struct {
	client SESAPI
	cfg    SESConfig
}

// Send implements email.Sender.
func (s *SES) Send(ctx context.Context, msg email.Message) (email.Result, error) {
	result := email.Result{Provider: SESName}
	in := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(sesAddress(msg.From)),
		Destination:      &types.Destination{},
		Content: &types.EmailContent{Simple: &types.Message{
			Subject: &types.Content{Data: aws.String(msg.Subject), Charset: aws.String("UTF-8")},
			Body:    &types.Body{},
		}},
		EmailTags: sesTags(msg),
	}
	for _, to := range msg.To {
		in.Destination.ToAddresses = append(in.Destination.ToAddresses, sesAddress(to))
	}
	if !msg.ReplyTo.IsZero() {
		in.ReplyToAddresses = []string{sesAddress(msg.ReplyTo)}
	}
	if msg.HTML != "" {
		in.Content.Simple.Body.Html = &types.Content{Data: aws.String(msg.HTML), Charset: aws.String("UTF-8")}
	}
	if msg.Text != "" {
		in.Content.Simple.Body.Text = &types.Content{Data: aws.String(msg.Text), Charset: aws.String("UTF-8")}
	}
	keys := make([]string, 0, len(msg.Headers))
	for key := range msg.Headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		in.Content.Simple.Headers = append(in.Content.Simple.Headers, types.MessageHeader{Name: aws.String(key), Value: aws.String(msg.Headers[key])})
	}
	if s.cfg.ConfigurationSet != "" {
		in.ConfigurationSetName = aws.String(s.cfg.ConfigurationSet)
	}
	out, err := s.client.SendEmail(ctx, in)
	if err != nil {
		return result, email.Classify(SESName, classifySES(err), fmt.Errorf("ses send: %w", err))
	}
	if out != nil && out.MessageId != nil {
		result.ProviderMessageID = *out.MessageId
	}
	return result, nil
}

func classifySES(err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "TooManyRequestsException", "Throttling", "ThrottlingException":
			return email.ErrRateLimited
		case "LimitExceededException", "SendingPausedException":
			return email.ErrQuotaExceeded
		case "MessageRejected":
			if strings.Contains(strings.ToLower(apiErr.ErrorMessage()), "address") {
				return email.ErrInvalidRecipient
			}
			return email.ErrPermanent
		case "BadRequestException", "NotFoundException", "AccountSuspendedException",
			"MailFromDomainNotVerifiedException", "AccessDeniedException":
			return email.ErrPermanent
		}
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() >= 400 && respErr.HTTPStatusCode() < 500 {
		return email.ErrPermanent
	}
	// 5xx, network errors, timeouts.
	return email.ErrTemporary
}

// sesTags turns Category and Tags into SES message tags (letters, digits,
// "_", "-" only).
func sesTags(msg email.Message) []types.MessageTag {
	var tags []types.MessageTag
	if msg.Category != "" {
		tags = append(tags, types.MessageTag{Name: aws.String("category"), Value: aws.String(tagValue(msg.Category))})
	}
	keys := make([]string, 0, len(msg.Tags))
	for key := range msg.Tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		tags = append(tags, types.MessageTag{Name: aws.String(tagValue(key)), Value: aws.String(tagValue(msg.Tags[key]))})
	}
	return tags
}

// sesAddress encodes a non-ASCII display name (RFC 2047), as SES requires.
func sesAddress(a email.Address) string {
	if a.Name == "" {
		return a.Address
	}
	return (&mail.Address{Name: a.Name, Address: a.Address}).String()
}
