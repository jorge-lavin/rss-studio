package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"rss-studio/internal/domain"
)

const (
	Provider                  = "stripe"
	defaultAPIBaseURL         = "https://api.stripe.com"
	defaultSignatureTolerance = 5 * time.Minute
	maxCheckoutResponseBytes  = 1 << 20
	maxWebhookPayloadBytes    = 1 << 20
)

var (
	ErrBillingDisabled     = errors.New("billing disabled")
	ErrCheckoutInvalid     = errors.New("invalid checkout request")
	ErrCheckoutUnavailable = errors.New("checkout unavailable")
	ErrWebhookInvalid      = errors.New("invalid webhook")
	ErrWebhookUnavailable  = errors.New("webhook unavailable")
)

// Config contains runtime-only Stripe settings. SecretKey and WebhookSecret
// are never included in errors or responses.
type Config struct {
	SecretKey          string
	WebhookSecret      string
	PriceID            string
	SuccessURL         string
	CancelURL          string
	APIBaseURL         string
	HTTPClient         *http.Client
	Now                func() time.Time
	SignatureTolerance time.Duration
}

func ConfigFromEnv() Config {
	return Config{
		SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		PriceID:       os.Getenv("STRIPE_PRICE_ID"),
		SuccessURL:    os.Getenv("STRIPE_SUCCESS_URL"),
		CancelURL:     os.Getenv("STRIPE_CANCEL_URL"),
		APIBaseURL:    defaultAPIBaseURL,
	}
}

type FeatureFlagStore interface {
	GetFeatureFlag(name string) (domain.FeatureFlag, error)
}

type WebhookStore interface {
	ApplyBillingWebhook(provider, eventID string, eventCreatedAt time.Time, state domain.BillingState) (bool, error)
}

type Service struct {
	config   Config
	flags    FeatureFlagStore
	webhooks WebhookStore
}

func New(config Config, flags FeatureFlagStore, webhooks WebhookStore) *Service {
	if config.APIBaseURL == "" {
		config.APIBaseURL = defaultAPIBaseURL
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.SignatureTolerance <= 0 {
		config.SignatureTolerance = defaultSignatureTolerance
	}
	return &Service{config: config, flags: flags, webhooks: webhooks}
}

// ValidateCheckout checks runtime rollout and provider configuration without
// making an external request. Callers use it before replaying cached results.
func (s *Service) ValidateCheckout() error {
	if s.flags == nil || !s.billingFlagsEnabled() {
		return ErrBillingDisabled
	}
	if s.config.SecretKey == "" || s.config.PriceID == "" || !validRedirectURL(s.config.SuccessURL) || !validRedirectURL(s.config.CancelURL) {
		return ErrCheckoutUnavailable
	}
	base, err := url.Parse(strings.TrimRight(s.config.APIBaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return ErrCheckoutUnavailable
	}
	return nil
}

func (s *Service) Checkout(ctx context.Context, accountID int64, productKey string) (string, error) {
	return s.CheckoutWithIdempotencyKey(ctx, accountID, productKey, "")
}

func (s *Service) CheckoutWithIdempotencyKey(ctx context.Context, accountID int64, productKey, idempotencyKey string) (string, error) {
	if accountID <= 0 {
		return "", ErrCheckoutInvalid
	}
	if productKey == "" {
		productKey = domain.BillingProductHostedListenerPersonal
	}
	product, ok := domain.BillingProductFor(productKey)
	if !ok || product.Key != domain.BillingProductHostedListenerPersonal {
		return "", ErrCheckoutInvalid
	}
	if err := s.ValidateCheckout(); err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("line_items[0][price]", s.config.PriceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("success_url", s.config.SuccessURL)
	form.Set("cancel_url", s.config.CancelURL)
	form.Set("client_reference_id", strconv.FormatInt(accountID, 10))
	form.Set("subscription_data[metadata][account_id]", strconv.FormatInt(accountID, 10))
	form.Set("subscription_data[metadata][product_key]", product.Key)
	if product.TrialDays > 0 {
		form.Set("subscription_data[trial_period_days]", strconv.Itoa(product.TrialDays))
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.config.APIBaseURL, "/")+"/v1/checkout/sessions", strings.NewReader(form.Encode()))
	if err != nil {
		return "", ErrCheckoutUnavailable
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(s.config.SecretKey, "")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := s.config.HTTPClient.Do(request)
	if err != nil {
		return "", ErrCheckoutUnavailable
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCheckoutResponseBytes+1))
	if err != nil || len(body) > maxCheckoutResponseBytes || response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", ErrCheckoutUnavailable
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &result); err != nil || !validCheckoutURL(result.URL) {
		return "", ErrCheckoutUnavailable
	}
	return result.URL, nil
}

func (s *Service) billingFlagsEnabled() bool {
	for _, name := range []string{domain.FeatureFlagBillingEnabled, domain.FeatureFlagBillingStripe} {
		flag, err := s.flags.GetFeatureFlag(name)
		if err != nil || !flag.Enabled {
			return false
		}
	}
	return true
}

type WebhookResult struct {
	Applied   bool
	Duplicate bool
}

func (s *Service) ProcessWebhook(signature string, payload []byte) (WebhookResult, error) {
	if s.config.WebhookSecret == "" {
		return WebhookResult{}, ErrWebhookUnavailable
	}
	if len(payload) == 0 || len(payload) > maxWebhookPayloadBytes {
		return WebhookResult{}, ErrWebhookInvalid
	}
	if err := verifySignature(signature, payload, s.config.WebhookSecret, s.config.Now(), s.config.SignatureTolerance); err != nil {
		return WebhookResult{}, err
	}
	if s.flags == nil || !s.billingFlagsEnabled() {
		return WebhookResult{}, ErrBillingDisabled
	}
	event, err := parseEvent(payload)
	if err != nil {
		return WebhookResult{}, err
	}
	if !supportedEvent(event.Type) {
		return WebhookResult{}, nil
	}
	if s.webhooks == nil {
		return WebhookResult{}, ErrWebhookUnavailable
	}
	state, err := billingStateForEvent(event)
	if err != nil {
		return WebhookResult{}, err
	}
	applied, err := s.webhooks.ApplyBillingWebhook(Provider, event.ID, time.Unix(event.Created, 0).UTC(), state)
	if err != nil {
		return WebhookResult{}, ErrWebhookUnavailable
	}
	return WebhookResult{Applied: applied, Duplicate: !applied}, nil
}

type eventEnvelope struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Data    struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

func parseEvent(payload []byte) (eventEnvelope, error) {
	var event eventEnvelope
	if err := json.Unmarshal(payload, &event); err != nil || event.ID == "" || event.Type == "" || event.Created <= 0 || len(event.ID) > 255 || len(event.Type) > 150 || len(event.Data.Object) == 0 {
		return eventEnvelope{}, ErrWebhookInvalid
	}
	return event, nil
}

func supportedEvent(eventType string) bool {
	switch eventType {
	case "checkout.session.completed", "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		return true
	default:
		return false
	}
}

type checkoutSession struct {
	ClientReferenceID string `json:"client_reference_id"`
	Customer          string `json:"customer"`
	Subscription      string `json:"subscription"`
	Mode              string `json:"mode"`
}

type subscriptionMetadata struct {
	AccountID  string `json:"account_id"`
	ProductKey string `json:"product_key"`
}

type subscription struct {
	ID                 string               `json:"id"`
	Customer           string               `json:"customer"`
	Status             string               `json:"status"`
	TrialStart         *int64               `json:"trial_start"`
	TrialEnd           *int64               `json:"trial_end"`
	CurrentPeriodStart int64                `json:"current_period_start"`
	CurrentPeriodEnd   int64                `json:"current_period_end"`
	Metadata           subscriptionMetadata `json:"metadata"`
}

func billingStateForEvent(event eventEnvelope) (domain.BillingState, error) {
	switch event.Type {
	case "checkout.session.completed":
		var session checkoutSession
		if err := json.Unmarshal(event.Data.Object, &session); err != nil || (session.Mode != "" && session.Mode != "subscription") || session.Subscription == "" {
			return domain.BillingState{}, ErrWebhookInvalid
		}
		accountID, err := parseAccountID(session.ClientReferenceID)
		if err != nil {
			return domain.BillingState{}, ErrWebhookInvalid
		}
		return domain.BillingState{
			AccountID: accountID, ProductKey: domain.BillingProductHostedListenerPersonal,
			Status: domain.BillingStatusPending, Provider: Provider,
			ProviderCustomerRef: session.Customer, ProviderSubscriptionRef: session.Subscription,
		}, nil
	default:
		var value subscription
		if err := json.Unmarshal(event.Data.Object, &value); err != nil || value.ID == "" {
			return domain.BillingState{}, ErrWebhookInvalid
		}
		accountID, err := parseAccountID(value.Metadata.AccountID)
		if err != nil || (value.Metadata.ProductKey != "" && value.Metadata.ProductKey != domain.BillingProductHostedListenerPersonal) {
			return domain.BillingState{}, ErrWebhookInvalid
		}
		productKey := domain.BillingProductHostedListenerPersonal
		if value.Metadata.ProductKey != "" {
			productKey = value.Metadata.ProductKey
		}
		status := billingStatus(value.Status, event.Type == "customer.subscription.deleted")
		state := domain.BillingState{
			AccountID: accountID, ProductKey: productKey, Status: status,
			Provider: Provider, ProviderCustomerRef: value.Customer, ProviderSubscriptionRef: value.ID,
		}
		if value.CurrentPeriodStart > 0 && value.CurrentPeriodEnd > 0 {
			start := time.Unix(value.CurrentPeriodStart, 0).UTC()
			end := time.Unix(value.CurrentPeriodEnd, 0).UTC()
			if !end.After(start) {
				return domain.BillingState{}, ErrWebhookInvalid
			}
			state.CurrentPeriodStart = &start
			state.CurrentPeriodEnd = &end
		}
		if value.TrialStart != nil && *value.TrialStart > 0 {
			trialStart := time.Unix(*value.TrialStart, 0).UTC()
			state.TrialStartedAt = &trialStart
		}
		if value.TrialEnd != nil && *value.TrialEnd > 0 {
			trialEnd := time.Unix(*value.TrialEnd, 0).UTC()
			state.TrialEndsAt = &trialEnd
		}
		return state, nil
	}
}

func parseAccountID(value string) (int64, error) {
	accountID, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || accountID <= 0 {
		return 0, ErrWebhookInvalid
	}
	return accountID, nil
}

func billingStatus(status string, deleted bool) domain.BillingStatus {
	if deleted {
		return domain.BillingStatusCanceled
	}
	switch status {
	case "trialing":
		return domain.BillingStatusTrialing
	case "active":
		return domain.BillingStatusActive
	case "past_due", "unpaid", "paused":
		return domain.BillingStatusPastDue
	case "canceled":
		return domain.BillingStatusCanceled
	case "incomplete_expired":
		return domain.BillingStatusExpired
	default:
		return domain.BillingStatusPending
	}
}

func verifySignature(header string, payload []byte, secret string, now time.Time, tolerance time.Duration) error {
	var timestamp int64
	var signatures [][]byte
	seenTimestamp := false
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || key == "" || value == "" {
			return ErrWebhookInvalid
		}
		switch key {
		case "t":
			if seenTimestamp {
				return ErrWebhookInvalid
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return ErrWebhookInvalid
			}
			timestamp, seenTimestamp = parsed, true
		case "v1":
			decoded, err := hex.DecodeString(value)
			if err == nil {
				signatures = append(signatures, decoded)
			}
		}
	}
	if !seenTimestamp || len(signatures) == 0 {
		return ErrWebhookInvalid
	}
	nowValue := now.UTC()
	created := time.Unix(timestamp, 0).UTC()
	delta := nowValue.Sub(created)
	if delta < 0 {
		delta = -delta
	}
	if delta > tolerance {
		return ErrWebhookInvalid
	}
	message := []byte(strconv.FormatInt(timestamp, 10) + "." + string(payload))
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write(message)
	expected := digest.Sum(nil)
	for _, signature := range signatures {
		if hmac.Equal(expected, signature) {
			return nil
		}
	}
	return ErrWebhookInvalid
}

func validRedirectURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.User == nil
}

func validCheckoutURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Host != "" && parsed.Scheme == "https" && parsed.User == nil && !strings.ContainsAny(value, "\r\n")
}
