package btcpay

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
	Provider               = "btcpay"
	WebhookSignatureHeader = "BTCPay-Sig"
	maxCheckoutResponse    = 1 << 20
	maxWebhookPayloadSize  = 1 << 20
)

var (
	ErrBillingDisabled     = errors.New("billing disabled")
	ErrCheckoutInvalid     = errors.New("invalid checkout request")
	ErrCheckoutUnavailable = errors.New("checkout unavailable")
	ErrWebhookInvalid      = errors.New("invalid webhook")
	ErrWebhookUnavailable  = errors.New("webhook unavailable")
)

// Config contains runtime-only BTCPay settings. API and webhook secrets are
// never included in errors or responses.
type Config struct {
	BaseURL       string
	APIKey        string
	WebhookSecret string
	StoreID       string
	OfferingID    string
	PlanID        string
	SuccessURL    string
	HTTPClient    *http.Client
}

func ConfigFromEnv() Config {
	baseURL := os.Getenv("BTCPAY_URL")
	if baseURL == "" {
		baseURL = os.Getenv("BTCPAY_BASE_URL")
	}
	return Config{
		BaseURL:       baseURL,
		APIKey:        os.Getenv("BTCPAY_API_KEY"),
		WebhookSecret: os.Getenv("BTCPAY_WEBHOOK_SECRET"),
		StoreID:       os.Getenv("BTCPAY_STORE_ID"),
		OfferingID:    os.Getenv("BTCPAY_OFFERING_ID"),
		PlanID:        os.Getenv("BTCPAY_PLAN_ID"),
		SuccessURL:    os.Getenv("BTCPAY_SUCCESS_URL"),
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
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Service{config: config, flags: flags, webhooks: webhooks}
}

// ValidateCheckout checks runtime rollout and provider configuration without
// making an external request. Callers use it before replaying cached results.
func (s *Service) ValidateCheckout() error {
	if s.flags == nil || !s.billingFlagsEnabled() {
		return ErrBillingDisabled
	}
	if s.config.APIKey == "" || s.config.StoreID == "" || s.config.OfferingID == "" || s.config.PlanID == "" {
		return ErrCheckoutUnavailable
	}
	base, err := url.Parse(strings.TrimRight(s.config.BaseURL, "/"))
	if err != nil || !validBaseURL(base) {
		return ErrCheckoutUnavailable
	}
	if s.config.SuccessURL != "" && !validRedirectURL(s.config.SuccessURL) {
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

	metadata := map[string]string{
		"account_id":  strconv.FormatInt(accountID, 10),
		"product_key": product.Key,
	}
	body := struct {
		StoreID               string            `json:"storeId"`
		OfferingID            string            `json:"offeringId"`
		PlanID                string            `json:"planId"`
		IsTrial               bool              `json:"isTrial"`
		NewSubscriberMetadata map[string]string `json:"newSubscriberMetadata"`
		Metadata              map[string]string `json:"metadata"`
		SuccessRedirectLink   string            `json:"successRedirectLink,omitempty"`
	}{
		StoreID:               s.config.StoreID,
		OfferingID:            s.config.OfferingID,
		PlanID:                s.config.PlanID,
		IsTrial:               product.TrialDays > 0,
		NewSubscriberMetadata: metadata,
		Metadata:              metadata,
		SuccessRedirectLink:   s.config.SuccessURL,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", ErrCheckoutUnavailable
	}
	base, _ := url.Parse(strings.TrimRight(s.config.BaseURL, "/"))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+"/api/v1/plan-checkout", strings.NewReader(string(encoded)))
	if err != nil {
		return "", ErrCheckoutUnavailable
	}
	request.Header.Set("Authorization", "token "+s.config.APIKey)
	request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := s.config.HTTPClient.Do(request)
	if err != nil {
		return "", ErrCheckoutUnavailable
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxCheckoutResponse+1))
	if err != nil || len(responseBody) > maxCheckoutResponse || response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", ErrCheckoutUnavailable
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil || !validCheckoutURL(result.URL) {
		return "", ErrCheckoutUnavailable
	}
	return result.URL, nil
}

func (s *Service) billingFlagsEnabled() bool {
	for _, name := range []string{domain.FeatureFlagBillingEnabled, domain.FeatureFlagBillingBitcoin} {
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
	if len(payload) == 0 || len(payload) > maxWebhookPayloadSize {
		return WebhookResult{}, ErrWebhookInvalid
	}
	if err := verifySignature(signature, payload, s.config.WebhookSecret); err != nil {
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
	state, err := s.billingStateForEvent(event)
	if err != nil {
		return WebhookResult{}, err
	}
	applied, err := s.webhooks.ApplyBillingWebhook(Provider, event.DeliveryID, time.Unix(event.Timestamp, 0).UTC(), state)
	if err != nil {
		return WebhookResult{}, ErrWebhookUnavailable
	}
	return WebhookResult{Applied: applied, Duplicate: !applied}, nil
}

type eventEnvelope struct {
	DeliveryID string          `json:"deliveryId"`
	Type       string          `json:"type"`
	Timestamp  int64           `json:"timestamp"`
	StoreID    string          `json:"storeId"`
	Reason     string          `json:"reason"`
	Subscriber json.RawMessage `json:"subscriber"`
}

func parseEvent(payload []byte) (eventEnvelope, error) {
	var event eventEnvelope
	if err := json.Unmarshal(payload, &event); err != nil || event.DeliveryID == "" || len(event.DeliveryID) > 255 || event.Type == "" || len(event.Type) > 100 || event.Timestamp <= 0 {
		return eventEnvelope{}, ErrWebhookInvalid
	}
	return event, nil
}

func supportedEvent(eventType string) bool {
	switch eventType {
	case "SubscriberCreated", "SubscriberCredited", "SubscriberCharged", "SubscriberActivated", "SubscriberPhaseChanged", "SubscriberDisabled", "PlanStarted", "SubscriberNeedUpgrade":
		return true
	default:
		return false
	}
}

type subscriber struct {
	Created  int64 `json:"created"`
	Customer struct {
		ID       string                     `json:"id"`
		Metadata map[string]json.RawMessage `json:"metadata"`
	} `json:"customer"`
	Offering struct {
		ID string `json:"id"`
	} `json:"offering"`
	Plan struct {
		ID string `json:"id"`
	} `json:"plan"`
	PeriodEnd   *int64                     `json:"periodEnd"`
	TrialEnd    *int64                     `json:"trialEnd"`
	IsActive    bool                       `json:"isActive"`
	IsSuspended bool                       `json:"isSuspended"`
	Phase       string                     `json:"phase"`
	Metadata    map[string]json.RawMessage `json:"metadata"`
}

func (s *Service) billingStateForEvent(event eventEnvelope) (domain.BillingState, error) {
	if len(event.Subscriber) == 0 || string(event.Subscriber) == "null" {
		return domain.BillingState{}, ErrWebhookInvalid
	}
	var value subscriber
	if err := json.Unmarshal(event.Subscriber, &value); err != nil || event.StoreID != s.config.StoreID || value.Customer.ID == "" || value.Offering.ID != s.config.OfferingID || value.Plan.ID != s.config.PlanID {
		return domain.BillingState{}, ErrWebhookInvalid
	}
	accountID, productKey, err := accountMetadata(value.Metadata, value.Customer.Metadata)
	if err != nil {
		return domain.BillingState{}, err
	}
	state := domain.BillingState{
		AccountID:               accountID,
		ProductKey:              productKey,
		Status:                  billingStatus(event.Type, value.Phase, value.IsActive, value.IsSuspended, event.Reason),
		Provider:                Provider,
		ProviderCustomerRef:     value.Customer.ID,
		ProviderSubscriptionRef: value.Customer.ID,
	}
	if value.Created > 0 && value.Phase == "Trial" {
		started := time.Unix(value.Created, 0).UTC()
		state.TrialStartedAt = &started
	}
	if value.TrialEnd != nil && *value.TrialEnd > 0 {
		trialEnd := time.Unix(*value.TrialEnd, 0).UTC()
		state.TrialEndsAt = &trialEnd
	}
	if value.PeriodEnd != nil && *value.PeriodEnd > 0 {
		periodEnd := time.Unix(*value.PeriodEnd, 0).UTC()
		state.CurrentPeriodEnd = &periodEnd
	}
	return state, nil
}

func accountMetadata(primary, secondary map[string]json.RawMessage) (int64, string, error) {
	accountValue := primary["account_id"]
	if len(accountValue) == 0 {
		accountValue = secondary["account_id"]
	}
	var accountIDString string
	if err := json.Unmarshal(accountValue, &accountIDString); err != nil {
		return 0, "", ErrWebhookInvalid
	}
	accountID, err := strconv.ParseInt(strings.TrimSpace(accountIDString), 10, 64)
	if err != nil || accountID <= 0 {
		return 0, "", ErrWebhookInvalid
	}
	productKey := domain.BillingProductHostedListenerPersonal
	productValue := primary["product_key"]
	if len(productValue) == 0 {
		productValue = secondary["product_key"]
	}
	if len(productValue) > 0 {
		if err := json.Unmarshal(productValue, &productKey); err != nil || productKey != domain.BillingProductHostedListenerPersonal {
			return 0, "", ErrWebhookInvalid
		}
	}
	return accountID, productKey, nil
}

func billingStatus(eventType, phase string, active, suspended bool, reason string) domain.BillingStatus {
	if eventType == "SubscriberDisabled" {
		if strings.EqualFold(reason, "Suspension") || suspended {
			return domain.BillingStatusPastDue
		}
		return domain.BillingStatusExpired
	}
	switch phase {
	case "Trial":
		return domain.BillingStatusTrialing
	case "Normal":
		return domain.BillingStatusActive
	case "Grace":
		return domain.BillingStatusPastDue
	case "Expired":
		return domain.BillingStatusExpired
	}
	if suspended {
		return domain.BillingStatusPastDue
	}
	if active {
		return domain.BillingStatusActive
	}
	return domain.BillingStatusPending
}

func verifySignature(header string, payload []byte, secret string) error {
	algorithm, encoded, ok := strings.Cut(strings.TrimSpace(header), "=")
	if !ok || algorithm != "sha256" || encoded == "" {
		return ErrWebhookInvalid
	}
	signature, err := hex.DecodeString(encoded)
	if err != nil {
		return ErrWebhookInvalid
	}
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write(payload)
	if !hmac.Equal(digest.Sum(nil), signature) {
		return ErrWebhookInvalid
	}
	return nil
}

func validBaseURL(value *url.URL) bool {
	return value != nil && value.Host != "" && value.User == nil && (value.Scheme == "http" || value.Scheme == "https") && !strings.ContainsAny(value.String(), "\r\n")
}

func validRedirectURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && parsed.Host != "" && parsed.User == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && !strings.ContainsAny(value, "\r\n")
}

func validCheckoutURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Host != "" && parsed.Scheme == "https" && parsed.User == nil && !strings.ContainsAny(value, "\r\n")
}
