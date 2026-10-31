package httpapi

import (
	"errors"
	"io"
	"net/http"

	"rss-studio/internal/application"
	"rss-studio/internal/domain"
	"rss-studio/internal/stripe"
)

type checkoutRequest struct {
	ProductKey string `json:"product_key"`
}

func (h *Handler) apiCreateCheckout(writer http.ResponseWriter, request *http.Request) {
	input := checkoutRequest{}
	if request.ContentLength != 0 {
		request.Body = http.MaxBytesReader(writer, request.Body, maxJSONRequestBody)
		if inputErr := parseAPIJSON(request, &input); inputErr != nil {
			writeAPIError(writer, request, http.StatusBadRequest, inputErr.Code, inputErr.Message)
			return
		}
	}
	if input.ProductKey == "" {
		input.ProductKey = domain.BillingProductHostedListenerPersonal
	}
	if h.stripe == nil {
		writeBillingCheckoutError(writer, request, stripe.ErrCheckoutUnavailable)
		return
	}
	key, inputErr := idempotencyKey(request)
	if inputErr != nil {
		writeAPIError(writer, request, http.StatusBadRequest, inputErr.Code, inputErr.Message)
		return
	}
	if err := h.stripe.ValidateCheckout(); err != nil {
		writeBillingCheckoutError(writer, request, err)
		return
	}
	requestHash, err := idempotencyHash("billing_checkout", input, key)
	if err != nil {
		writeAPIError(writer, request, http.StatusInternalServerError, "internal_error", "Could not create checkout.")
		return
	}
	result, err := h.service.ExecuteIdempotent(request.Context(), h.requestUser(request), requestCredentialHash(request), key, requestHash, func() (any, error) {
		checkoutURL, err := h.stripe.CheckoutWithIdempotencyKey(request.Context(), h.requestUser(request).ID, input.ProductKey, key)
		if err != nil {
			return nil, err
		}
		return struct {
			CheckoutURL string `json:"checkout_url"`
		}{CheckoutURL: checkoutURL}, nil
	})
	if err != nil {
		writeBillingCheckoutError(writer, request, err)
		return
	}
	writeAPIJSONBytes(writer, http.StatusOK, result)
}

func writeBillingCheckoutError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, stripe.ErrCheckoutInvalid):
		writeAPIError(writer, request, http.StatusBadRequest, "validation_error", "Checkout request is invalid.")
	case errors.Is(err, application.ErrIdempotencyConflict):
		writeAPIError(writer, request, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used with a different request.")
	case errors.Is(err, application.ErrIdempotencyUnavailable):
		writeAPIError(writer, request, http.StatusServiceUnavailable, "idempotency_unavailable", "Idempotency protection is temporarily unavailable.")
	default:
		writeAPIError(writer, request, http.StatusServiceUnavailable, "billing_unavailable", "Billing is temporarily unavailable.")
	}
}

func (h *Handler) apiCheckoutMethodNotAllowed(writer http.ResponseWriter, request *http.Request) {
	h.apiMethodNotAllowedFor(writer, request, http.MethodPost)
}

func (h *Handler) stripeWebhook(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		h.apiMethodNotAllowedFor(writer, request, http.MethodPost)
		return
	}
	if h.stripe == nil {
		writeAPIError(writer, request, http.StatusServiceUnavailable, "billing_unavailable", "Billing is temporarily unavailable.")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		writeAPIError(writer, request, http.StatusBadRequest, "invalid_webhook", "Invalid webhook.")
		return
	}
	if _, err := h.stripe.ProcessWebhook(request.Header.Get("Stripe-Signature"), payload); err != nil {
		status, code, message := http.StatusBadRequest, "invalid_webhook", "Invalid webhook."
		if errors.Is(err, stripe.ErrWebhookUnavailable) || errors.Is(err, stripe.ErrBillingDisabled) {
			status, code, message = http.StatusServiceUnavailable, "billing_unavailable", "Billing is temporarily unavailable."
		}
		writeAPIError(writer, request, status, code, message)
		return
	}
	writeAPIJSON(writer, http.StatusOK, struct {
		Received bool `json:"received"`
	}{Received: true})
}
