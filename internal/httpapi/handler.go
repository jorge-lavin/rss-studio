package httpapi

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"rss-studio/internal/application"
	"rss-studio/internal/domain"
	"rss-studio/internal/observability"
	"rss-studio/internal/policy"
	"rss-studio/internal/storage"
	"rss-studio/internal/version"
)

type Handler struct {
	service      *application.Service
	auth         *Authenticator
	loginLimiter *loginRateLimiter
	apiLimiter   *apiRateLimiter
}

type homePageData struct {
	Feeds         []domain.Feed
	BaseURL       string
	Version       string
	Error         string
	Authenticated bool
}

type authPageData struct {
	Error        string
	Registration bool
}

type authCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type previewRequest struct {
	SourceURL string          `json:"source_url"`
	Outputs   []previewOutput `json:"outputs"`
}

type createFeedRequest struct {
	Name      string                `json:"name"`
	SourceURL string                `json:"source_url"`
	Operator  domain.FilterOperator `json:"operator"`
	Keyword   string                `json:"keyword"`
}

type previewOutput struct {
	Name     string                `json:"name"`
	Operator domain.FilterOperator `json:"operator"`
	Keyword  string                `json:"keyword"`
}

type previewResponse struct {
	Outputs []previewResponseOutput `json:"outputs"`
}

type previewResponseOutput struct {
	Name         string   `json:"name"`
	Operator     string   `json:"operator"`
	Keyword      string   `json:"keyword"`
	Count        int      `json:"count"`
	SampleTitles []string `json:"sample_titles"`
}

type feedListResponse struct {
	Feeds []feedListResponseFeed `json:"feeds"`
}

type feedListResponseFeed struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	URL       string    `json:"url"`
	SourceURL string    `json:"source_url"`
	Operator  string    `json:"operator"`
	Keyword   string    `json:"keyword"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type splitResponse struct {
	Outputs []splitResponseOutput `json:"outputs"`
}

type splitResponseOutput struct {
	Name     string `json:"name"`
	Operator string `json:"operator"`
	Keyword  string `json:"keyword"`
	Path     string `json:"path"`
	URL      string `json:"url"`
	Status   string `json:"status"`
}

type serviceTokenCreateRequest struct {
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresIn int64      `json:"expires_in"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type serviceTokenResponse struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type createdServiceTokenResponse struct {
	serviceTokenResponse
	Token string `json:"token"`
}

const (
	maxJSONRequestBody     = 1 << 20
	maxAuthJSONRequestBody = 16 << 10
	maxIdempotencyKey      = 255
)

func NewHandler(service *application.Service, authenticators ...*Authenticator) http.Handler {
	return NewHandlerWithClock(service, time.Now, authenticators...)
}

// NewHandlerWithClock builds the HTTP handler with an injectable clock for
// deterministic rate-limit tests and single-instance policy checks.
func NewHandlerWithClock(service *application.Service, now func() time.Time, authenticators ...*Authenticator) http.Handler {
	var auth *Authenticator
	if len(authenticators) > 0 {
		auth = authenticators[0]
	}
	handler := &Handler{
		service: service, auth: auth, loginLimiter: newLoginRateLimiterWithClock(now),
		apiLimiter: newAPIRateLimiter(service.RateLimitStore(), now),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handler.home)
	if auth != nil {
		mux.HandleFunc("GET /auth/sign-in", handler.loginPage)
		mux.HandleFunc("GET /auth/login", handler.loginPage)
		mux.HandleFunc("POST /auth/sign-in", handler.login)
		mux.HandleFunc("POST /auth/login", handler.login)
		mux.HandleFunc("POST /auth/logout", handler.logout)
		mux.HandleFunc("GET /auth/sign-up", handler.registerPage)
		mux.HandleFunc("GET /auth/register", handler.registerPage)
		mux.HandleFunc("POST /auth/sign-up", handler.register)
		mux.HandleFunc("POST /auth/register", handler.register)
		mux.HandleFunc("POST /api/v1/auth/login", handler.apiLogin)
		mux.HandleFunc("POST /api/v1/auth/register", handler.apiRegister)
		mux.HandleFunc("GET /api/v1/auth/tokens", handler.apiListServiceTokens)
		mux.HandleFunc("POST /api/v1/auth/tokens", handler.apiCreateServiceToken)
		mux.HandleFunc("POST /api/v1/auth/tokens/{id}/revoke", handler.apiRevokeServiceToken)
		mux.HandleFunc("DELETE /api/v1/auth/tokens/{id}", handler.apiRevokeServiceToken)
		mux.HandleFunc("/api/v1/auth/login", handler.apiMethodNotAllowed)
		mux.HandleFunc("/api/v1/auth/register", handler.apiMethodNotAllowed)
		mux.HandleFunc("GET /api/v1/feeds", handler.apiListFeeds)
		mux.HandleFunc("POST /api/v1/feeds", handler.apiCreateFeed)
		mux.HandleFunc("PUT /api/v1/feeds", handler.apiFeedsMethodNotAllowed)
		mux.HandleFunc("PATCH /api/v1/feeds", handler.apiFeedsMethodNotAllowed)
		mux.HandleFunc("DELETE /api/v1/feeds", handler.apiFeedsMethodNotAllowed)
		mux.HandleFunc("POST /api/v1/feeds/preview", handler.apiPreview)
		mux.HandleFunc("POST /api/v1/feeds/split", handler.apiSplitFeeds)
		mux.HandleFunc("DELETE /api/v1/feeds/{path}", handler.apiDeleteFeed)
		mux.HandleFunc("GET /api/v1/feeds/{path}", handler.apiFeedPathMethodNotAllowed)
		mux.HandleFunc("POST /api/v1/feeds/{path}", handler.apiFeedPathMethodNotAllowed)
		mux.HandleFunc("PUT /api/v1/feeds/{path}", handler.apiFeedPathMethodNotAllowed)
		mux.HandleFunc("PATCH /api/v1/feeds/{path}", handler.apiFeedPathMethodNotAllowed)
		mux.HandleFunc("GET /api/v1/feeds/split", handler.apiSplitMethodNotAllowed)
		mux.HandleFunc("PUT /api/v1/feeds/split", handler.apiSplitMethodNotAllowed)
		mux.HandleFunc("PATCH /api/v1/feeds/split", handler.apiSplitMethodNotAllowed)
		mux.HandleFunc("GET /api/v1/feeds/preview", handler.apiPreviewMethodNotAllowed)
		mux.HandleFunc("PUT /api/v1/feeds/preview", handler.apiPreviewMethodNotAllowed)
		mux.HandleFunc("PATCH /api/v1/feeds/preview", handler.apiPreviewMethodNotAllowed)
	}
	mux.HandleFunc("GET /api", handler.apiPage)
	mux.HandleFunc("GET /api/{$}", handler.apiPage)
	mux.HandleFunc("/api/v1", handler.apiNotFound)
	mux.HandleFunc("/api/v1/", handler.apiNotFound)
	mux.HandleFunc("POST /feeds", handler.createFeed)
	mux.HandleFunc("POST /feeds/split", handler.splitFeeds)
	mux.HandleFunc("POST /preview", handler.preview)
	mux.HandleFunc("GET /feeds/{path}", handler.serveFeed)
	mux.HandleFunc("POST /feeds/{path}/delete", handler.deleteFeed)
	return observability.HTTPHandler(handler.authenticate(mux))
}

func (h *Handler) apiLogin(writer http.ResponseWriter, request *http.Request) {
	if allowed, retryAfter := h.loginLimiter.allow(requestClient(request)); !allowed {
		seconds := int((retryAfter + time.Second - 1) / time.Second)
		writer.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeAPIError(writer, request, http.StatusTooManyRequests, "rate_limited", "Too many login attempts. Try again later.")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxAuthJSONRequestBody)
	credentials, inputErr := parseAPICredentials(request)
	if inputErr != nil {
		writeAPIError(writer, request, http.StatusBadRequest, inputErr.Code, inputErr.Message)
		return
	}
	token, expires, err := h.auth.IssueToken(credentials.Username, credentials.Password)
	if err != nil {
		writeAPIError(writer, request, http.StatusUnauthorized, "invalid_credentials", "Invalid credentials.")
		return
	}
	writeAPIJSON(writer, http.StatusOK, struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresAt   int64  `json:"expires_at"`
	}{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresAt:   expires.Unix(),
	})
}

func (h *Handler) apiRegister(writer http.ResponseWriter, request *http.Request) {
	if allowed, retryAfter := h.loginLimiter.allow(requestClient(request)); !allowed {
		seconds := int((retryAfter + time.Second - 1) / time.Second)
		writer.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeAPIError(writer, request, http.StatusTooManyRequests, "rate_limited", "Too many registration attempts. Try again later.")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxAuthJSONRequestBody)
	credentials, inputErr := parseAPICredentials(request)
	if inputErr != nil {
		writeAPIError(writer, request, http.StatusBadRequest, inputErr.Code, inputErr.Message)
		return
	}
	if err := h.auth.Register(credentials.Username, credentials.Password); err != nil {
		status := http.StatusInternalServerError
		code := "internal_error"
		message := "Could not create account."
		switch {
		case errors.Is(err, domain.ErrInvalidUser):
			status = http.StatusBadRequest
			code = "validation_error"
			message = "Username and password are required."
		case errors.Is(err, domain.ErrUsernameTaken):
			status = http.StatusConflict
			code = "username_taken"
			message = "Username already exists."
		case errors.Is(err, ErrRegistrationUnavailable):
			status = http.StatusNotImplemented
			code = "registration_unavailable"
			message = "Registration is unavailable."
		default:
			log.Printf("register user: %v", err)
		}
		writeAPIError(writer, request, status, code, message)
		return
	}
	writeAPIJSON(writer, http.StatusCreated, struct {
		Username string `json:"username"`
	}{Username: strings.TrimSpace(credentials.Username)})
}

func (h *Handler) apiCreateServiceToken(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	result := "error"
	defer func() {
		observability.Record(request.Context(), observability.Event{
			Event:      observability.EventMutation,
			Operation:  policy.OperationTokensCreate,
			Result:     result,
			DurationMS: time.Since(started).Milliseconds(),
		})
	}()
	request.Body = http.MaxBytesReader(writer, request.Body, maxAuthJSONRequestBody)
	var input serviceTokenCreateRequest
	if inputErr := parseAPIJSON(request, &input); inputErr != nil {
		writeAPIError(writer, request, http.StatusBadRequest, inputErr.Code, inputErr.Message)
		return
	}
	if (input.ExpiresIn <= 0) == (input.ExpiresAt == nil) {
		writeAPIError(writer, request, http.StatusBadRequest, "validation_error", "Exactly one token expiry is required.")
		return
	}
	expiresAt := time.Time{}
	if input.ExpiresAt != nil {
		expiresAt = input.ExpiresAt.UTC()
	} else {
		maxDurationSeconds := int64((time.Duration(1<<63 - 1)) / time.Second)
		if input.ExpiresIn > maxDurationSeconds {
			writeAPIError(writer, request, http.StatusBadRequest, "validation_error", "Token expiry is invalid.")
			return
		}
		expiresAt = h.auth.now().UTC().Add(time.Duration(input.ExpiresIn) * time.Second)
	}
	token, raw, err := h.auth.CreateServiceToken(h.requestUser(request), input.Name, input.Scopes, expiresAt)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidServiceToken) || errors.Is(err, ErrServiceTokenUnavailable) {
			status := http.StatusBadRequest
			code := "validation_error"
			message := "Token name, scopes, and expiry are invalid."
			if errors.Is(err, ErrServiceTokenUnavailable) {
				status = http.StatusNotImplemented
				code = "service_tokens_unavailable"
				message = "Service tokens are unavailable."
			}
			writeAPIError(writer, request, status, code, message)
			return
		}
		log.Printf("create service token: %v", err)
		writeAPIError(writer, request, http.StatusInternalServerError, "internal_error", "Could not create service token.")
		return
	}
	writeAPIJSON(writer, http.StatusCreated, createdServiceTokenResponse{
		serviceTokenResponse: serviceTokenResponseFor(token),
		Token:                raw,
	})
	result = "success"
}

func (h *Handler) apiListServiceTokens(writer http.ResponseWriter, request *http.Request) {
	tokens, err := h.auth.ListServiceTokens(h.requestUser(request))
	if err != nil {
		if errors.Is(err, ErrServiceTokenUnavailable) {
			writeAPIError(writer, request, http.StatusNotImplemented, "service_tokens_unavailable", "Service tokens are unavailable.")
			return
		}
		log.Printf("list service tokens: %v", err)
		writeAPIError(writer, request, http.StatusInternalServerError, "internal_error", "Could not list service tokens.")
		return
	}
	response := make([]serviceTokenResponse, len(tokens))
	for i, token := range tokens {
		response[i] = serviceTokenResponseFor(token)
	}
	writeAPIJSON(writer, http.StatusOK, struct {
		Tokens []serviceTokenResponse `json:"tokens"`
	}{Tokens: response})
}

func (h *Handler) apiRevokeServiceToken(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	result := "error"
	defer func() {
		observability.Record(request.Context(), observability.Event{
			Event:      observability.EventMutation,
			Operation:  policy.OperationTokensRevoke,
			Result:     result,
			DurationMS: time.Since(started).Milliseconds(),
		})
	}()
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(writer, request, http.StatusBadRequest, "validation_error", "Token ID is invalid.")
		return
	}
	if err := h.auth.RevokeServiceToken(h.requestUser(request), id); err != nil {
		if errors.Is(err, storage.ErrServiceTokenNotFound) {
			writeAPIError(writer, request, http.StatusNotFound, "not_found", "Service token not found.")
			return
		}
		if errors.Is(err, ErrServiceTokenUnavailable) {
			writeAPIError(writer, request, http.StatusNotImplemented, "service_tokens_unavailable", "Service tokens are unavailable.")
			return
		}
		log.Printf("revoke service token: %v", err)
		writeAPIError(writer, request, http.StatusInternalServerError, "internal_error", "Could not revoke service token.")
		return
	}
	writeAPIJSON(writer, http.StatusOK, struct {
		Revoked bool  `json:"revoked"`
		ID      int64 `json:"id"`
	}{Revoked: true, ID: id})
	result = "success"
}

func serviceTokenResponseFor(token domain.ServiceToken) serviceTokenResponse {
	return serviceTokenResponse{
		ID: token.ID, Name: token.Name, Scopes: token.Scopes,
		CreatedAt: token.CreatedAt, ExpiresAt: token.ExpiresAt, RevokedAt: token.RevokedAt,
	}
}

func (h *Handler) apiMethodNotAllowed(writer http.ResponseWriter, request *http.Request) {
	h.apiMethodNotAllowedFor(writer, request, http.MethodPost)
}

func (h *Handler) apiFeedsMethodNotAllowed(writer http.ResponseWriter, request *http.Request) {
	h.apiMethodNotAllowedFor(writer, request, "GET, POST")
}

func (h *Handler) apiFeedPathMethodNotAllowed(writer http.ResponseWriter, request *http.Request) {
	h.apiMethodNotAllowedFor(writer, request, http.MethodDelete)
}

func (h *Handler) apiSplitMethodNotAllowed(writer http.ResponseWriter, request *http.Request) {
	h.apiMethodNotAllowedFor(writer, request, http.MethodPost)
}

func (h *Handler) apiPreviewMethodNotAllowed(writer http.ResponseWriter, request *http.Request) {
	h.apiMethodNotAllowedFor(writer, request, http.MethodPost)
}

func (h *Handler) apiMethodNotAllowedFor(writer http.ResponseWriter, request *http.Request, method string) {
	writer.Header().Set("Allow", method)
	writeAPIError(writer, request, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
}

func (h *Handler) apiNotFound(writer http.ResponseWriter, request *http.Request) {
	writeAPIError(writer, request, http.StatusNotFound, "not_found", "API endpoint not found.")
}

func (h *Handler) apiListFeeds(writer http.ResponseWriter, request *http.Request) {
	feeds, err := h.service.ListFeeds(h.requestUser(request))
	if err != nil {
		log.Printf("list API feeds: %v", err)
		writeAPIError(writer, request, http.StatusInternalServerError, "internal_error", "Could not list feeds.")
		return
	}

	response := feedListResponse{Feeds: make([]feedListResponseFeed, len(feeds))}
	for i, feed := range feeds {
		response.Feeds[i] = h.feedListResponseFeedFor(request, feed)
	}
	writeAPIJSON(writer, http.StatusOK, response)
}

func (h *Handler) apiCreateFeed(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxJSONRequestBody)
	var input createFeedRequest
	if inputErr := parseAPIJSON(request, &input); inputErr != nil {
		writeAPIError(writer, request, http.StatusBadRequest, inputErr.Code, inputErr.Message)
		return
	}

	key, inputErr := idempotencyKey(request)
	if inputErr != nil {
		writeAPIError(writer, request, http.StatusBadRequest, inputErr.Code, inputErr.Message)
		return
	}
	requestHash, err := idempotencyHash("create_feed", input, key)
	if err != nil {
		writeAPIError(writer, request, http.StatusInternalServerError, "internal_error", "Could not create feed.")
		return
	}
	result, err := h.service.ExecuteIdempotent(request.Context(), h.requestUser(request), requestCredentialHash(request), key, requestHash, func() (any, error) {
		feed, err := h.service.CreateFeedWithOperator(request.Context(), h.requestUser(request), input.Name, input.SourceURL, input.Operator, input.Keyword)
		if err != nil {
			return nil, err
		}
		return h.feedListResponseFeedFor(request, feed), nil
	})
	if err != nil {
		h.writeAPIFeedError(writer, request, err, "Could not create feed.")
		return
	}

	writeAPIJSONBytes(writer, http.StatusCreated, result)
}

func (h *Handler) apiPreview(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxJSONRequestBody)
	var input previewRequest
	if inputErr := parseAPIJSON(request, &input); inputErr != nil {
		writeAPIError(writer, request, http.StatusBadRequest, inputErr.Code, inputErr.Message)
		return
	}

	rules := previewRulesFor(input.Outputs)
	results, err := h.service.PreviewForOwner(request.Context(), h.requestUser(request), input.SourceURL, rules)
	if err != nil {
		switch {
		case errors.Is(err, application.ErrQuotaExceeded):
			var quotaErr *application.QuotaError
			if errors.As(err, &quotaErr) {
				writeAPIQuotaError(writer, request, quotaErr)
				return
			}
			writeAPIError(writer, request, http.StatusTooManyRequests, "quota_exceeded", "Quota exceeded.")
		case errors.Is(err, application.ErrQuotaUnavailable):
			writeAPIError(writer, request, http.StatusServiceUnavailable, "quota_unavailable", "Quotas are temporarily unavailable.")
		case errors.Is(err, application.ErrInvalidFeed):
			writeAPIError(writer, request, http.StatusBadRequest, "validation_error", err.Error())
		case errors.Is(err, application.ErrSourceUnavailable):
			writeAPIError(writer, request, http.StatusBadGateway, "source_unavailable", "Source feed could not be fetched or processed.")
		default:
			log.Printf("preview API feed: %v", err)
			writeAPIError(writer, request, http.StatusInternalServerError, "internal_error", "Could not preview feed.")
		}
		return
	}

	writeAPIJSON(writer, http.StatusOK, previewResponseFor(results))
}

func (h *Handler) apiSplitFeeds(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxJSONRequestBody)
	var input previewRequest
	if inputErr := parseAPIJSON(request, &input); inputErr != nil {
		writeAPIError(writer, request, http.StatusBadRequest, inputErr.Code, inputErr.Message)
		return
	}

	key, inputErr := idempotencyKey(request)
	if inputErr != nil {
		writeAPIError(writer, request, http.StatusBadRequest, inputErr.Code, inputErr.Message)
		return
	}
	requestHash, err := idempotencyHash("split_feeds", input, key)
	if err != nil {
		writeAPIError(writer, request, http.StatusInternalServerError, "internal_error", "Could not publish feeds.")
		return
	}
	result, err := h.service.ExecuteIdempotent(request.Context(), h.requestUser(request), requestCredentialHash(request), key, requestHash, func() (any, error) {
		feeds, err := h.service.CreateSplitFeeds(request.Context(), h.requestUser(request), input.SourceURL, previewRulesFor(input.Outputs))
		if err != nil {
			return nil, err
		}
		return h.splitResponseFor(request, feeds), nil
	})
	if err != nil {
		h.writeAPIFeedError(writer, request, err, "Could not publish feeds.")
		return
	}

	writeAPIJSONBytes(writer, http.StatusCreated, result)
}

func (h *Handler) writeAPIFeedError(writer http.ResponseWriter, request *http.Request, err error, internalMessage string) {
	switch {
	case errors.Is(err, application.ErrInvalidFeed):
		writeAPIError(writer, request, http.StatusBadRequest, "validation_error", err.Error())
	case errors.Is(err, application.ErrQuotaExceeded):
		var quotaErr *application.QuotaError
		if errors.As(err, &quotaErr) {
			writeAPIQuotaError(writer, request, quotaErr)
			return
		}
		writeAPIError(writer, request, http.StatusTooManyRequests, "quota_exceeded", "Quota exceeded.")
	case errors.Is(err, application.ErrQuotaUnavailable):
		writeAPIError(writer, request, http.StatusServiceUnavailable, "quota_unavailable", "Quotas are temporarily unavailable.")
	case errors.Is(err, application.ErrFeedNotFound):
		writeAPIError(writer, request, http.StatusNotFound, "not_found", "Feed not found.")
	case errors.Is(err, application.ErrSourceUnavailable):
		writeAPIError(writer, request, http.StatusBadGateway, "source_unavailable", "Source feed could not be fetched or processed.")
	case errors.Is(err, application.ErrIdempotencyConflict):
		writeAPIError(writer, request, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used with a different request.")
	default:
		log.Printf("API feed operation: %v", err)
		writeAPIError(writer, request, http.StatusInternalServerError, "internal_error", internalMessage)
	}
}

func writeAPIQuotaError(writer http.ResponseWriter, request *http.Request, quotaErr *application.QuotaError) {
	if quotaErr == nil {
		writeAPIError(writer, request, http.StatusTooManyRequests, "quota_exceeded", "Quota exceeded.")
		return
	}
	if quotaErr.ResetAfter > 0 {
		writer.Header().Set("Retry-After", strconv.Itoa(rateLimitSeconds(quotaErr.ResetAfter)))
	}
	var resetAt *time.Time
	if !quotaErr.ResetAt.IsZero() {
		value := quotaErr.ResetAt.UTC()
		resetAt = &value
	}
	resetAfter := 0
	if quotaErr.ResetAfter > 0 {
		resetAfter = rateLimitSeconds(quotaErr.ResetAfter)
	}
	writeAPIJSON(writer, http.StatusTooManyRequests, apiErrorResponse{
		Error: apiErrorBody{
			Code: "quota_exceeded", Message: "Quota exceeded.", Quota: string(quotaErr.Kind),
			Limit: quotaErr.Limit, Remaining: quotaErr.Remaining,
			ResetAt: resetAt, ResetAfter: &resetAfter,
		},
		RequestID: observability.RequestID(request),
	})
}

func idempotencyKey(request *http.Request) (string, *apiInputError) {
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" {
		return "", nil
	}
	if len(key) > maxIdempotencyKey {
		return "", &apiInputError{Code: "validation_error", Message: "Idempotency key is invalid."}
	}
	for _, character := range key {
		if unicode.IsControl(character) {
			return "", &apiInputError{Code: "validation_error", Message: "Idempotency key is invalid."}
		}
	}
	return key, nil
}

func idempotencyHash(operation string, request any, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	return application.IdempotencyRequestHash(operation, request)
}

func requestCredentialHash(request *http.Request) string {
	identity, ok := IdentityFromContext(request.Context())
	if !ok {
		return ""
	}
	return identity.CredentialHash
}

func previewRulesFor(outputs []previewOutput) []application.PreviewRule {
	rules := make([]application.PreviewRule, len(outputs))
	for i, output := range outputs {
		rules[i] = application.PreviewRule{Name: output.Name, Operator: output.Operator, Keyword: output.Keyword}
	}
	return rules
}

func (h *Handler) login(writer http.ResponseWriter, request *http.Request) {
	if allowed, retryAfter := h.loginLimiter.allow(requestClient(request)); !allowed {
		seconds := int((retryAfter + time.Second - 1) / time.Second)
		writer.Header().Set("Retry-After", strconv.Itoa(seconds))
		if !isJSONRequest(request) {
			h.renderLogin(writer, "Too many login attempts. Try again later.", http.StatusTooManyRequests)
			return
		}
		http.Error(writer, "Too many login attempts. Try again later.", http.StatusTooManyRequests)
		return
	}

	if isJSONRequest(request) {
		request.Body = http.MaxBytesReader(writer, request.Body, maxAuthJSONRequestBody)
	}
	credentials, err := parseCredentials(request)
	if err != nil {
		http.Error(writer, "Invalid login request.", http.StatusBadRequest)
		return
	}

	token, expires, err := h.auth.IssueToken(credentials.Username, credentials.Password)
	if err != nil {
		if !isJSONRequest(request) {
			h.renderLogin(writer, "Invalid credentials.", http.StatusUnauthorized)
			return
		}
		http.Error(writer, "Invalid credentials.", http.StatusUnauthorized)
		return
	}
	if !isJSONRequest(request) {
		http.SetCookie(writer, &http.Cookie{
			Name:     authCookieName,
			Value:    token,
			Path:     "/",
			Expires:  expires,
			HttpOnly: true,
			Secure:   request.TLS != nil || strings.EqualFold(request.Header.Get("X-Forwarded-Proto"), "https"),
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(writer, request, "/", http.StatusSeeOther)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(writer).Encode(struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresAt   int64  `json:"expires_at"`
	}{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresAt:   expires.Unix(),
	})
}

func (h *Handler) logout(writer http.ResponseWriter, request *http.Request) {
	http.SetCookie(writer, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   request.TLS != nil || strings.EqualFold(request.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(writer, request, "/auth/login", http.StatusSeeOther)
}

func (h *Handler) register(writer http.ResponseWriter, request *http.Request) {
	if allowed, retryAfter := h.loginLimiter.allow(requestClient(request)); !allowed {
		seconds := int((retryAfter + time.Second - 1) / time.Second)
		writer.Header().Set("Retry-After", strconv.Itoa(seconds))
		if !isJSONRequest(request) {
			h.renderRegister(writer, "Too many registration attempts. Try again later.", http.StatusTooManyRequests)
			return
		}
		http.Error(writer, "Too many registration attempts. Try again later.", http.StatusTooManyRequests)
		return
	}

	if isJSONRequest(request) {
		request.Body = http.MaxBytesReader(writer, request.Body, maxAuthJSONRequestBody)
	}
	credentials, err := parseCredentials(request)
	if err != nil {
		http.Error(writer, "Invalid registration request.", http.StatusBadRequest)
		return
	}
	if err := h.auth.Register(credentials.Username, credentials.Password); err != nil {
		status := http.StatusInternalServerError
		message := "Could not create account."
		switch {
		case errors.Is(err, domain.ErrInvalidUser):
			status = http.StatusBadRequest
			message = "Username and password are required."
		case errors.Is(err, domain.ErrUsernameTaken):
			status = http.StatusConflict
			message = "Username already exists."
		case errors.Is(err, ErrRegistrationUnavailable):
			status = http.StatusNotImplemented
		default:
			log.Printf("register user: %v", err)
		}
		if !isJSONRequest(request) {
			h.renderRegister(writer, message, status)
			return
		}
		http.Error(writer, message, status)
		return
	}

	if isJSONRequest(request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(struct {
			Username string `json:"username"`
		}{Username: strings.TrimSpace(credentials.Username)})
		return
	}
	http.Redirect(writer, request, "/auth/login", http.StatusSeeOther)
}

func parseCredentials(request *http.Request) (authCredentials, error) {
	var credentials authCredentials
	if isJSONRequest(request) {
		decoder := json.NewDecoder(request.Body)
		if err := decoder.Decode(&credentials); err != nil {
			return authCredentials{}, err
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return authCredentials{}, err
		}
		return credentials, nil
	}
	if err := request.ParseForm(); err != nil {
		return authCredentials{}, err
	}
	credentials.Username = request.FormValue("username")
	credentials.Password = request.FormValue("password")
	return credentials, nil
}

func (h *Handler) loginPage(writer http.ResponseWriter, request *http.Request) {
	h.renderLogin(writer, "", http.StatusOK)
}

func (h *Handler) registerPage(writer http.ResponseWriter, request *http.Request) {
	h.renderRegister(writer, "", http.StatusOK)
}

func (h *Handler) renderLogin(writer http.ResponseWriter, message string, status int) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	if err := authTemplate.Execute(writer, authPageData{Error: message}); err != nil {
		log.Printf("render login: %v", err)
	}
}

func (h *Handler) renderRegister(writer http.ResponseWriter, message string, status int) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	if err := authTemplate.Execute(writer, authPageData{Error: message, Registration: true}); err != nil {
		log.Printf("render login: %v", err)
	}
}

func isJSONRequest(request *http.Request) bool {
	return strings.HasPrefix(request.Header.Get("Content-Type"), "application/json")
}

func (h *Handler) authenticate(next http.Handler) http.Handler {
	if h.auth == nil {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isPublicRequest(request) {
			next.ServeHTTP(writer, request)
			return
		}
		identity, err := h.auth.IdentityFromRequest(request)
		if err != nil {
			if isAPIRequest(request) {
				writer.Header().Set("WWW-Authenticate", `Bearer realm="rss-studio"`)
				writeAPIError(writer, request, http.StatusUnauthorized, "authentication_required", "Authentication required.")
				return
			}
			if request.Method == http.MethodGet && acceptsHTML(request) {
				http.Redirect(writer, request, "/auth/login", http.StatusSeeOther)
				return
			}
			writer.Header().Set("WWW-Authenticate", `Bearer realm="rss-studio"`)
			http.Error(writer, "Authentication required.", http.StatusUnauthorized)
			return
		}
		request = request.WithContext(withIdentity(request.Context(), identity))
		operation, hasOperation := operationNameForRequest(request)
		if hasOperation {
			observability.SetOperationAttributes(request.Context(), operation, identity.User.ID, identity.TokenID)
		}
		policyStarted := time.Now()
		if requiredScope, requiresScope := requiredScopeForRequest(request); requiresScope {
			if err := policy.Authorize(identity, requiredScope); err != nil {
				if !hasOperation {
					operation = "request"
				}
				observability.Record(request.Context(), observability.Event{
					Event:      observability.EventPolicy,
					Operation:  operation,
					Result:     "denied",
					DurationMS: time.Since(policyStarted).Milliseconds(),
				})
				writePolicyError(writer, request, err)
				return
			}
		}
		if isAPIRequest(request) {
			if hasOperation {
				observability.Record(request.Context(), observability.Event{
					Event:      observability.EventPolicy,
					Operation:  operation,
					Result:     "allowed",
					DurationMS: time.Since(policyStarted).Milliseconds(),
				})
				decision, err := h.apiLimiter.allow(identity, operation)
				if err != nil {
					observability.Record(request.Context(), observability.Event{
						Event:      observability.EventRateLimit,
						Operation:  operation,
						Result:     "unavailable",
						DurationMS: time.Since(policyStarted).Milliseconds(),
					})
					log.Printf("rate limit check: %v", err)
					writeAPIError(writer, request, http.StatusServiceUnavailable, "rate_limit_unavailable", "Rate limiting is temporarily unavailable.")
					return
				}
				if h.apiLimiter.store != nil {
					setRateLimitHeaders(writer, decision)
				}
				if !decision.Allowed {
					observability.Record(request.Context(), observability.Event{
						Event:        observability.EventRateLimit,
						Operation:    operation,
						Result:       "rejected",
						DurationMS:   time.Since(policyStarted).Milliseconds(),
						RateRejected: true,
					})
					rateLimitError(writer, request, decision)
					return
				}
			}
		}
		next.ServeHTTP(writer, request)
	})
}

func requiredScopeForRequest(request *http.Request) (string, bool) {
	if isAPIRequest(request) {
		switch {
		case strings.HasPrefix(request.URL.Path, "/api/v1/auth/tokens"):
			return domain.ScopeTokensManage, true
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/feeds":
			return domain.ScopeFeedsRead, true
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/feeds":
			return domain.ScopeFeedsWrite, true
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/feeds/preview":
			return domain.ScopeFeedsPreview, true
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/feeds/split":
			return domain.ScopeFeedsWrite, true
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/api/v1/feeds/"):
			return domain.ScopeFeedsDelete, true
		}
		return "", false
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/":
		return domain.ScopeFeedsRead, true
	case request.Method == http.MethodPost && request.URL.Path == "/feeds":
		return domain.ScopeFeedsWrite, true
	case request.Method == http.MethodPost && request.URL.Path == "/feeds/split":
		return domain.ScopeFeedsWrite, true
	case request.Method == http.MethodPost && request.URL.Path == "/preview":
		return domain.ScopeFeedsPreview, true
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/feeds/") && strings.HasSuffix(request.URL.Path, "/delete"):
		return domain.ScopeFeedsDelete, true
	default:
		return "", false
	}
}

func writeAPIPolicyError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, policy.ErrInsufficientScope):
		writer.Header().Set("WWW-Authenticate", `Bearer realm="rss-studio", error="insufficient_scope"`)
		writeAPIError(writer, request, http.StatusForbidden, "insufficient_scope", "Insufficient scope.")
	default:
		writeAPIError(writer, request, http.StatusForbidden, "forbidden", "This credential cannot access this endpoint.")
	}
}

func writePolicyError(writer http.ResponseWriter, request *http.Request, err error) {
	if isAPIRequest(request) {
		writeAPIPolicyError(writer, request, err)
		return
	}
	http.Error(writer, "This credential cannot access this endpoint.", http.StatusForbidden)
}

func acceptsHTML(request *http.Request) bool {
	return strings.Contains(strings.ToLower(request.Header.Get("Accept")), "text/html")
}

func isPublicRequest(request *http.Request) bool {
	if request.Method == http.MethodGet && (request.URL.Path == "/api" || request.URL.Path == "/api/") {
		return true
	}
	if request.URL.Path == "/auth/login" || request.URL.Path == "/auth/sign-in" || request.URL.Path == "/auth/register" || request.URL.Path == "/auth/sign-up" {
		return request.Method == http.MethodGet || request.Method == http.MethodPost
	}
	if request.URL.Path == "/api/v1/auth/login" || request.URL.Path == "/api/v1/auth/register" {
		return true
	}
	if request.URL.Path == "/auth/logout" {
		return request.Method == http.MethodPost
	}
	return request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/feeds/") && strings.HasSuffix(request.URL.Path, ".xml")
}

func (h *Handler) home(writer http.ResponseWriter, request *http.Request) {
	h.renderHome(writer, request, "", http.StatusOK)
}

func (h *Handler) createFeed(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		h.renderHome(writer, request, "Could not read form.", http.StatusBadRequest)
		return
	}
	_, err := h.service.CreateFeedWithOperator(request.Context(), h.requestUser(request), request.FormValue("name"), request.FormValue("source_url"), domain.FilterOperator(request.FormValue("operator")), request.FormValue("keyword"))
	if err != nil {
		switch {
		case errors.Is(err, application.ErrQuotaExceeded):
			h.renderHome(writer, request, "Quota exceeded. Try again later.", http.StatusTooManyRequests)
		case errors.Is(err, application.ErrQuotaUnavailable):
			h.renderHome(writer, request, "Quotas are temporarily unavailable.", http.StatusServiceUnavailable)
		case errors.Is(err, application.ErrInvalidFeed):
			h.renderHome(writer, request, err.Error(), http.StatusBadRequest)
		case errors.Is(err, application.ErrSourceUnavailable):
			h.renderHome(writer, request, "Source feed could not be fetched or processed.", http.StatusBadGateway)
		default:
			log.Printf("create feed: %v", err)
			h.renderHome(writer, request, "Could not save feed.", http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(writer, request, "/", http.StatusSeeOther)
}

func (h *Handler) preview(writer http.ResponseWriter, request *http.Request) {
	var input previewRequest
	request.Body = http.MaxBytesReader(writer, request.Body, maxJSONRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeJSONError(writer, "Invalid preview request.", http.StatusBadRequest)
		return
	}

	rules := make([]application.PreviewRule, len(input.Outputs))
	for i, output := range input.Outputs {
		rules[i] = application.PreviewRule{Name: output.Name, Operator: output.Operator, Keyword: output.Keyword}
	}
	results, err := h.service.PreviewForOwner(request.Context(), h.requestUser(request), input.SourceURL, rules)
	if err != nil {
		switch {
		case errors.Is(err, application.ErrQuotaExceeded):
			writeJSONError(writer, "Quota exceeded. Try again later.", http.StatusTooManyRequests)
		case errors.Is(err, application.ErrQuotaUnavailable):
			writeJSONError(writer, "Quotas are temporarily unavailable.", http.StatusServiceUnavailable)
		case errors.Is(err, application.ErrInvalidFeed):
			writeJSONError(writer, err.Error(), http.StatusBadRequest)
		case errors.Is(err, application.ErrSourceUnavailable):
			writeJSONError(writer, "Source feed could not be fetched or processed.", http.StatusBadGateway)
		default:
			log.Printf("preview feed: %v", err)
			writeJSONError(writer, "Could not preview feed.", http.StatusInternalServerError)
		}
		return
	}

	response := previewResponseFor(results)
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		log.Printf("write preview response: %v", err)
	}
}

func previewResponseFor(results []application.PreviewResult) previewResponse {
	response := previewResponse{Outputs: make([]previewResponseOutput, len(results))}
	for i, result := range results {
		response.Outputs[i] = previewResponseOutput{
			Name:         result.Name,
			Operator:     string(result.Operator),
			Keyword:      result.Keyword,
			Count:        result.Count,
			SampleTitles: result.Titles,
		}
	}
	return response
}

func (h *Handler) feedListResponseFeedFor(request *http.Request, feed domain.Feed) feedListResponseFeed {
	path := "/feeds/" + feed.Path + ".xml"
	return feedListResponseFeed{
		Name: feed.Name, Path: path, URL: baseURL(request) + path,
		SourceURL: feed.SourceURL, Operator: string(feed.Operator), Keyword: feed.Keyword,
		Status: string(h.service.RefreshStatus(feed)), CreatedAt: feed.CreatedAt,
	}
}

func (h *Handler) splitFeeds(writer http.ResponseWriter, request *http.Request) {
	var input previewRequest
	request.Body = http.MaxBytesReader(writer, request.Body, maxJSONRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeJSONError(writer, "Invalid split request.", http.StatusBadRequest)
		return
	}

	rules := make([]application.PreviewRule, len(input.Outputs))
	for i, output := range input.Outputs {
		rules[i] = application.PreviewRule{Name: output.Name, Operator: output.Operator, Keyword: output.Keyword}
	}
	feeds, err := h.service.CreateSplitFeeds(request.Context(), h.requestUser(request), input.SourceURL, rules)
	if err != nil {
		switch {
		case errors.Is(err, application.ErrQuotaExceeded):
			writeJSONError(writer, "Quota exceeded. Try again later.", http.StatusTooManyRequests)
		case errors.Is(err, application.ErrQuotaUnavailable):
			writeJSONError(writer, "Quotas are temporarily unavailable.", http.StatusServiceUnavailable)
		case errors.Is(err, application.ErrInvalidFeed):
			writeJSONError(writer, err.Error(), http.StatusBadRequest)
		case errors.Is(err, application.ErrSourceUnavailable):
			writeJSONError(writer, "Source feed could not be fetched or processed.", http.StatusBadGateway)
		default:
			log.Printf("split feeds: %v", err)
			writeJSONError(writer, "Could not publish feeds.", http.StatusInternalServerError)
		}
		return
	}

	response := h.splitResponseFor(request, feeds)
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		log.Printf("write split response: %v", err)
	}
}

func (h *Handler) splitResponseFor(request *http.Request, feeds []domain.Feed) splitResponse {
	response := splitResponse{Outputs: make([]splitResponseOutput, len(feeds))}
	for i, feed := range feeds {
		path := "/feeds/" + feed.Path + ".xml"
		response.Outputs[i] = splitResponseOutput{
			Name:     feed.Name,
			Operator: string(feed.Operator),
			Keyword:  feed.Keyword,
			Path:     path,
			URL:      baseURL(request) + path,
			Status:   string(h.service.RefreshStatus(feed)),
		}
	}
	return response
}

func writeJSONError(writer http.ResponseWriter, message string, status int) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}

func (h *Handler) serveFeed(writer http.ResponseWriter, request *http.Request) {
	file := request.PathValue("path")
	if !strings.HasSuffix(file, ".xml") {
		http.NotFound(writer, request)
		return
	}
	data, err := h.service.CachedFeed(request.Context(), strings.TrimSuffix(file, ".xml"))
	if err != nil {
		switch {
		case errors.Is(err, application.ErrFeedNotFound):
			http.NotFound(writer, request)
		case errors.Is(err, application.ErrCacheNotReady):
			http.Error(writer, "Feed cache is not ready yet.", http.StatusServiceUnavailable)
		default:
			log.Printf("serve feed: %T", err)
			http.Error(writer, "Could not read feed.", http.StatusInternalServerError)
		}
		return
	}
	decision, err := h.apiLimiter.allowPublicFeed(request.URL.Path)
	if err != nil {
		log.Printf("public feed rate limit unavailable")
		writeAPIError(writer, request, http.StatusServiceUnavailable, "rate_limit_unavailable", "Rate limiting is temporarily unavailable.")
		return
	}
	setRateLimitHeaders(writer, decision)
	if !decision.Allowed {
		rateLimitError(writer, request, decision)
		return
	}
	writer.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	writer.Header().Set("Cache-Control", "public, max-age=300")
	writer.Write(data)
}

func (h *Handler) deleteFeed(writer http.ResponseWriter, request *http.Request) {
	if err := h.service.DeleteFeedContext(request.Context(), h.requestUser(request), request.PathValue("path")); err != nil {
		if errors.Is(err, application.ErrFeedNotFound) {
			http.NotFound(writer, request)
			return
		}
		log.Printf("delete feed: %v", err)
		http.Error(writer, "Could not delete feed.", http.StatusInternalServerError)
		return
	}
	http.Redirect(writer, request, "/", http.StatusSeeOther)
}

func (h *Handler) apiDeleteFeed(writer http.ResponseWriter, request *http.Request) {
	path := feedPath(request.PathValue("path"))
	if err := h.service.DeleteFeedContext(request.Context(), h.requestUser(request), path); err != nil {
		h.writeAPIFeedError(writer, request, err, "Could not delete feed.")
		return
	}
	writeAPIJSON(writer, http.StatusOK, struct {
		Deleted bool   `json:"deleted"`
		Path    string `json:"path"`
	}{Deleted: true, Path: path})
}

func feedPath(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "/feeds/")
	return strings.TrimSuffix(value, ".xml")
}

func (h *Handler) renderHome(writer http.ResponseWriter, request *http.Request, message string, status int) {
	feeds, err := h.service.ListFeeds(h.requestUser(request))
	if err != nil {
		log.Printf("list feeds: %v", err)
		if message == "" {
			message = "Could not read feeds."
		}
		if status == http.StatusOK {
			status = http.StatusInternalServerError
		}
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	if err := homeTemplate.Execute(writer, homePageData{Feeds: feeds, BaseURL: baseURL(request), Version: version.Version, Error: message, Authenticated: h.auth != nil}); err != nil {
		log.Printf("render home: %v", err)
	}
}

func (h *Handler) requestUser(request *http.Request) domain.User {
	if user, ok := userFromContext(request.Context()); ok {
		return user
	}
	return domain.User{Username: "anonymous"}
}

func baseURL(request *http.Request) string {
	scheme := request.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
		if request.TLS != nil {
			scheme = "https"
		}
	}
	return scheme + "://" + request.Host
}

var homeTemplate = template.Must(template.New("home").Parse(`<!doctype html>
<html lang="en" class="h-full bg-slate-100 dark:bg-slate-950">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <script>
    (() => {
      try {
        const preference = localStorage.getItem("rss-studio-theme");
        const dark = preference === "dark" || (preference !== "light" && window.matchMedia("(prefers-color-scheme: dark)").matches);
        document.documentElement.classList.toggle("dark", dark);
        document.documentElement.style.colorScheme = dark ? "dark" : "light";
      } catch {}
    })();
  </script>
  <title>RSS Studio</title>
  <script src="https://cdn.tailwindcss.com"></script>
  <script>
    tailwind.config = { darkMode: "class" };
  </script>
</head>
<body class="min-h-full bg-slate-100 text-slate-950 antialiased dark:bg-slate-950 dark:text-slate-100">
<div class="min-h-screen">
  <header class="bg-slate-950 text-white">
    <div class="mx-auto max-w-6xl px-5 sm:px-8">
      <nav class="flex items-center justify-between border-b border-white/10 py-5">
        <a href="/" class="flex items-center gap-2 text-base font-semibold tracking-tight">
          <span class="flex h-8 w-8 items-center justify-center rounded-lg bg-violet-600 text-sm font-black text-white">R</span>
          RSS Studio
        </a>
        <div class="flex items-center gap-4">
          <a href="/api" class="text-xs font-medium text-slate-300 transition hover:text-white">API</a>
          {{if .Authenticated}}<form method="post" action="/auth/logout"><button type="submit" class="inline-flex items-center gap-2 rounded-lg border border-white/10 bg-white/5 px-3 py-2 text-xs font-medium text-slate-300 transition hover:bg-white/10 focus:outline-none focus:ring-4 focus:ring-violet-400/30">Logout</button></form>{{end}}
          <button id="theme-toggle" type="button" aria-pressed="false" aria-label="Switch to dark mode" class="inline-flex items-center gap-2 rounded-lg border border-white/10 bg-white/5 px-3 py-2 text-xs font-medium text-slate-300 transition hover:bg-white/10 focus:outline-none focus:ring-4 focus:ring-violet-400/30">
            <span data-theme-icon aria-hidden="true">☾</span><span data-theme-label>Dark mode</span>
          </button>
        </div>
      </nav>
    </div>
  </header>

  <main class="mx-auto max-w-6xl px-5 pb-16 sm:px-8">
    <section class="mt-10 rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800 sm:p-8">
      <div class="mb-7 flex flex-col gap-2 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h2 class="text-xl font-semibold tracking-tight">Create a filtered feed</h2>
        </div>
        <span class="text-xs font-medium uppercase tracking-wider text-slate-400">New feed</span>
      </div>
      {{if .Error}}<div class="mb-6 flex items-start gap-3 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700 dark:border-rose-800 dark:bg-rose-950/50 dark:text-rose-300" role="alert"><span class="mt-0.5 font-bold">!</span><p>{{.Error}}</p></div>{{end}}
      <form method="post" action="/feeds" class="grid gap-5 lg:grid-cols-[1fr_1.4fr_0.8fr_0.8fr_auto] lg:items-end">
        <div>
          <label for="name" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Feed name</label>
          <input id="name" name="name" maxlength="120" placeholder="My tech episodes" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
        </div>
        <div>
          <label for="source_url" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Source RSS URL</label>
          <input id="source_url" name="source_url" type="url" maxlength="2048" placeholder="https://example.com/feed.xml" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
        </div>
        <div>
          <label for="operator" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Operator</label>
          <select id="operator" name="operator" class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100">
            <option value="contains">Contains</option>
            <option value="starts_with">Starts with</option>
            <option value="ends_with">Ends with</option>
            <option value="equals">Equals</option>
          </select>
        </div>
        <div>
          <label for="keyword" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Match title</label>
          <input id="keyword" name="keyword" maxlength="200" placeholder="e.g. AI" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
        </div>
        <button type="submit" class="inline-flex min-h-[46px] items-center justify-center gap-2 rounded-lg bg-violet-600 px-5 py-3 text-sm font-semibold text-white shadow-sm transition hover:bg-violet-700 focus:outline-none focus:ring-4 focus:ring-violet-600/20 lg:mb-0">Publish feed <span aria-hidden="true">→</span></button>
      </form>
    </section>

    <section class="mt-8 rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800 sm:p-8" aria-labelledby="split-heading">
      <div class="mb-7 flex flex-col gap-2 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h2 id="split-heading" class="text-xl font-semibold tracking-tight">Split feeds</h2>
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Create multiple filtered feeds from one source.</p>
        </div>
        <span class="text-xs font-medium uppercase tracking-wider text-slate-400">Batch feeds</span>
      </div>
      <form id="split-form" method="post" action="/feeds/split" class="space-y-6">
        <div>
          <label for="split-source-url" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Source RSS URL</label>
          <input id="split-source-url" name="source_url" type="url" maxlength="2048" placeholder="https://example.com/feed.xml" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
        </div>
        <div>
          <div class="mb-3 flex items-center justify-between gap-4">
            <h3 class="text-sm font-semibold text-slate-800 dark:text-slate-200">Output feeds</h3>
            <button id="add-split-output" type="button" aria-disabled="false" aria-label="Add output" class="text-sm font-semibold text-violet-600 hover:text-violet-700 focus:outline-none focus:ring-4 focus:ring-violet-600/10 disabled:cursor-not-allowed disabled:opacity-50 dark:text-violet-400 dark:hover:text-violet-300">Add output <span aria-hidden="true">＋</span></button>
          </div>
          <div id="split-outputs" class="space-y-4">
             <div data-output-row class="grid gap-4 rounded-xl border border-slate-200 p-4 dark:border-slate-800 sm:grid-cols-[1fr_0.8fr_1fr_auto] sm:items-end">
              <div>
                <label for="split-output-name-1" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Output name</label>
                <input id="split-output-name-1" data-output-name name="output_name" maxlength="120" placeholder="AI episodes" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
              </div>
               <div>
                 <label for="split-output-operator-1" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Operator</label>
                 <select id="split-output-operator-1" data-output-operator name="output_operator" class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100">
                   <option value="contains">Contains</option>
                   <option value="starts_with">Starts with</option>
                   <option value="ends_with">Ends with</option>
                   <option value="equals">Equals</option>
                 </select>
               </div>
               <div>
                 <label for="split-output-keyword-1" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Match title</label>
                <input id="split-output-keyword-1" data-output-keyword name="output_keyword" maxlength="200" placeholder="e.g. AI" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
              </div>
              <button type="button" data-remove-output disabled aria-disabled="true" aria-label="Remove output 1" class="min-h-[46px] rounded-lg border border-slate-300 px-4 py-3 text-sm font-medium text-slate-500 transition hover:border-rose-300 hover:text-rose-600 focus:outline-none focus:ring-4 focus:ring-violet-600/10 disabled:cursor-not-allowed disabled:opacity-50 dark:border-slate-700 dark:text-slate-400 dark:hover:border-rose-700 dark:hover:text-rose-400">Remove</button>
            </div>
          </div>
          <template id="split-output-row-template">
             <div data-output-row class="grid gap-4 rounded-xl border border-slate-200 p-4 dark:border-slate-800 sm:grid-cols-[1fr_0.8fr_1fr_auto] sm:items-end">
              <div>
                <label data-output-name-label class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Output name</label>
                <input data-output-name name="output_name" maxlength="120" placeholder="AI episodes" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
              </div>
               <div>
                 <label data-output-operator-label class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Operator</label>
                 <select data-output-operator name="output_operator" class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100">
                   <option value="contains">Contains</option>
                   <option value="starts_with">Starts with</option>
                   <option value="ends_with">Ends with</option>
                   <option value="equals">Equals</option>
                 </select>
               </div>
               <div>
                 <label data-output-keyword-label class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Match title</label>
                <input data-output-keyword name="output_keyword" maxlength="200" placeholder="e.g. AI" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
              </div>
              <button type="button" data-remove-output aria-label="Remove output 2" class="min-h-[46px] rounded-lg border border-slate-300 px-4 py-3 text-sm font-medium text-slate-500 transition hover:border-rose-300 hover:text-rose-600 focus:outline-none focus:ring-4 focus:ring-violet-600/10 disabled:cursor-not-allowed disabled:opacity-50 dark:border-slate-700 dark:text-slate-400 dark:hover:border-rose-700 dark:hover:text-rose-400">Remove</button>
            </div>
          </template>
        </div>
        <div id="split-status" role="status" aria-live="polite" aria-atomic="true" class="space-y-3 text-sm text-slate-600 dark:text-slate-300"></div>
        <div class="flex flex-wrap gap-3">
          <button id="split-preview" type="button" class="inline-flex min-h-[46px] items-center justify-center rounded-lg border border-violet-600 px-5 py-3 text-sm font-semibold text-violet-700 shadow-sm transition hover:bg-violet-50 focus:outline-none focus:ring-4 focus:ring-violet-600/20 dark:text-violet-300 dark:hover:bg-violet-950/50">Preview split</button>
          <button id="split-publish" type="submit" class="inline-flex min-h-[46px] items-center justify-center gap-2 rounded-lg bg-violet-600 px-5 py-3 text-sm font-semibold text-white shadow-sm transition hover:bg-violet-700 focus:outline-none focus:ring-4 focus:ring-violet-600/20">Publish split feeds <span aria-hidden="true">→</span></button>
        </div>
      </form>
    </section>

    <section class="mt-12">
      <div class="mb-5 flex items-center justify-between gap-4">
        <div>
          <h2 class="text-xl font-semibold tracking-tight">Published feeds</h2>
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Ready to add to any RSS reader.</p>
        </div>
        <span class="rounded-full bg-slate-200 px-3 py-1 text-sm font-medium text-slate-600 dark:bg-slate-800 dark:text-slate-300">{{len .Feeds}}</span>
      </div>
      {{if not .Feeds}}
      <div class="rounded-2xl border border-dashed border-slate-300 bg-white px-6 py-14 text-center dark:border-slate-700 dark:bg-slate-900">
        <div class="mx-auto flex h-11 w-11 items-center justify-center rounded-full bg-violet-50 text-xl text-violet-600 dark:bg-violet-950 dark:text-violet-300">＋</div>
        <h3 class="mt-4 text-sm font-semibold text-slate-900 dark:text-slate-100">No filtered feeds yet</h3>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Create your first one above.</p>
      </div>
      {{else}}
      <div class="space-y-3">
        {{range .Feeds}}
        <article class="rounded-2xl border border-slate-200 bg-white p-5 shadow-sm dark:border-slate-800 dark:bg-slate-900 sm:p-6">
          <div class="flex flex-col gap-5 lg:flex-row lg:items-center lg:justify-between">
            <div class="min-w-0">
              <div class="flex flex-wrap items-center gap-3">
                <h3 class="text-base font-semibold text-slate-900 dark:text-slate-100">{{.Name}}</h3>
                <span class="rounded-full bg-violet-50 px-2.5 py-1 text-xs font-medium text-violet-700 dark:bg-violet-950 dark:text-violet-300">{{.Operator.Label}} “{{.Keyword}}”</span>
              </div>
              <p class="mt-2 truncate text-sm text-slate-500 dark:text-slate-400" title="{{.SourceURL}}">Source: {{.SourceURL}}</p>
              <a href="/feeds/{{.Path}}.xml" class="mt-3 block truncate text-sm font-medium text-violet-600 hover:text-violet-700 hover:underline dark:text-violet-400 dark:hover:text-violet-300" title="{{$.BaseURL}}/feeds/{{.Path}}.xml">{{$.BaseURL}}/feeds/{{.Path}}.xml</a>
            </div>
            <div class="flex shrink-0 items-center gap-4 border-t border-slate-100 pt-4 dark:border-slate-800 lg:border-0 lg:pt-0">
              <a href="/feeds/{{.Path}}.xml" class="text-sm font-semibold text-slate-700 hover:text-slate-950 dark:text-slate-300 dark:hover:text-slate-100">Open feed <span aria-hidden="true">↗</span></a>
              <form method="post" action="/feeds/{{.Path}}/delete"><button class="text-sm font-medium text-rose-600 hover:text-rose-700 dark:text-rose-400 dark:hover:text-rose-300" type="submit">Delete</button></form>
            </div>
          </div>
        </article>
        {{end}}
      </div>
      {{end}}
    </section>
  </main>

  <footer class="mx-auto max-w-6xl px-5 pb-8 text-xs text-slate-400 sm:px-8">RSS Studio · Version {{.Version}}</footer>
</div>
<script>
  (() => {
    const root = document.documentElement;
    const toggle = document.getElementById("theme-toggle");
    if (!toggle) return;
    const icon = toggle.querySelector("[data-theme-icon]");
    const label = toggle.querySelector("[data-theme-label]");
    const update = () => {
      const dark = root.classList.contains("dark");
      root.style.colorScheme = dark ? "dark" : "light";
      toggle.setAttribute("aria-pressed", String(dark));
      toggle.setAttribute("aria-label", dark ? "Switch to light mode" : "Switch to dark mode");
      icon.textContent = dark ? "☀" : "☾";
      label.textContent = dark ? "Light mode" : "Dark mode";
    };
    toggle.addEventListener("click", () => {
      const dark = !root.classList.contains("dark");
      root.classList.toggle("dark", dark);
      try {
        localStorage.setItem("rss-studio-theme", dark ? "dark" : "light");
      } catch {}
      update();
    });
    update();
  })();
</script>
<script>
  (() => {
    const form = document.getElementById("split-form");
    if (!form) return;
    const outputs = document.getElementById("split-outputs");
    const rowTemplate = document.getElementById("split-output-row-template");
    const addButton = document.getElementById("add-split-output");
    const previewButton = document.getElementById("split-preview");
    const status = document.getElementById("split-status");
    let busy = false;
    let nextOutputID = 2;
    const maxOutputs = 32;

    const clearStatus = () => {
      status.replaceChildren();
      status.classList.remove("text-rose-700", "dark:text-rose-300");
    };
    const showMessage = (message, error) => {
      clearStatus();
      status.textContent = message;
      if (error) status.classList.add("text-rose-700", "dark:text-rose-300");
    };
    const updateRemoveButtons = () => {
      const canRemove = outputs.children.length > 1;
      outputs.querySelectorAll("[data-remove-output]").forEach((button, index) => {
        const name = button.closest("[data-output-row]").querySelector("[data-output-name]").value.trim();
        button.setAttribute("aria-label", name ? "Remove output " + name : "Remove output " + (index + 1));
        button.disabled = busy || !canRemove;
        button.setAttribute("aria-disabled", String(busy || !canRemove));
      });
      const atLimit = outputs.children.length >= maxOutputs;
      addButton.disabled = busy || atLimit;
      addButton.setAttribute("aria-disabled", String(busy || atLimit));
      addButton.setAttribute("aria-label", atLimit ? "Output limit reached" : "Add output");
    };
    const setBusy = (value) => {
      busy = value;
      form.querySelectorAll("input, button").forEach((control) => { control.disabled = value; });
      updateRemoveButtons();
      status.setAttribute("aria-busy", String(value));
    };
     const payload = () => ({
       source_url: form.querySelector('[name="source_url"]').value.trim(),
       outputs: Array.from(outputs.querySelectorAll("[data-output-row]")).map((row) => ({
         name: row.querySelector("[data-output-name]").value.trim(),
         operator: row.querySelector("[data-output-operator]").value,
         keyword: row.querySelector("[data-output-keyword]").value.trim()
       }))
    });
    const addOutput = () => {
      if (busy || outputs.children.length >= maxOutputs) return;
      const fragment = rowTemplate.content.cloneNode(true);
       const row = fragment.querySelector("[data-output-row]");
       const number = nextOutputID++;
       const nameID = "split-output-name-" + number;
       const operatorID = "split-output-operator-" + number;
       const keywordID = "split-output-keyword-" + number;
       row.querySelector("[data-output-name-label]").htmlFor = nameID;
       row.querySelector("[data-output-operator-label]").htmlFor = operatorID;
       row.querySelector("[data-output-keyword-label]").htmlFor = keywordID;
       row.querySelector("[data-output-name]").id = nameID;
       row.querySelector("[data-output-operator]").id = operatorID;
       row.querySelector("[data-output-keyword]").id = keywordID;
      outputs.appendChild(row);
      updateRemoveButtons();
    };
    const renderPreview = (data) => {
      clearStatus();
      const heading = document.createElement("p");
      heading.className = "font-semibold text-slate-800 dark:text-slate-200";
      heading.textContent = "Preview results";
      status.appendChild(heading);
      const results = data && Array.isArray(data.outputs) ? data.outputs : [];
      results.forEach((output) => {
        const section = document.createElement("section");
        const title = document.createElement("p");
        const name = typeof output.name === "string" ? output.name : "Output";
        const count = Number.isFinite(output.count) ? output.count : 0;
        title.textContent = name + ": " + count + " matching episode" + (count === 1 ? "" : "s");
        section.appendChild(title);
        const samples = Array.isArray(output.sample_titles) ? output.sample_titles.slice(0, 5) : [];
        if (samples.length === 0) {
          const empty = document.createElement("p");
          empty.className = "text-slate-500 dark:text-slate-400";
          empty.textContent = "No matching episode titles.";
          section.appendChild(empty);
        } else {
          const list = document.createElement("ul");
          list.className = "list-disc pl-5 text-slate-500 dark:text-slate-400";
          samples.forEach((sample) => {
            const item = document.createElement("li");
            item.textContent = typeof sample === "string" ? sample : String(sample);
            list.appendChild(item);
          });
          section.appendChild(list);
        }
        status.appendChild(section);
      });
    };
    const renderPublished = (data) => {
      clearStatus();
      const heading = document.createElement("p");
      heading.className = "font-semibold text-slate-800 dark:text-slate-200";
      heading.textContent = "Published split feeds";
      status.appendChild(heading);
      const list = document.createElement("ul");
      list.className = "space-y-1 text-slate-500 dark:text-slate-400";
      const results = data && Array.isArray(data.outputs) ? data.outputs : [];
      results.forEach((output) => {
        const item = document.createElement("li");
        const name = typeof output.name === "string" ? output.name : "Output";
        const url = typeof output.url === "string" ? output.url : "";
        item.textContent = name + (url ? ": " + url : "");
        list.appendChild(item);
      });
      status.appendChild(list);
    };
    const send = async (endpoint, message, render) => {
      if (busy || !form.reportValidity()) return;
      setBusy(true);
      showMessage(message, false);
      try {
        const response = await fetch(endpoint, {
          method: "POST",
          credentials: "same-origin",
          headers: { "Content-Type": "application/json", "Accept": "application/json" },
          body: JSON.stringify(payload())
        });
        const data = await response.json().catch(() => null);
        if (!response.ok) {
          const message = data && typeof data.error === "string" && data.error ? data.error : "Request failed (HTTP " + response.status + ").";
          throw new Error(message);
        }
        render(data);
      } catch (error) {
        showMessage(error instanceof Error ? error.message : "Request failed.", true);
      } finally {
        setBusy(false);
      }
    };

    addButton.addEventListener("click", addOutput);
    outputs.addEventListener("click", (event) => {
      const target = event.target;
      if (!(target instanceof Element) || !target.matches("[data-remove-output]") || target.disabled || busy) return;
      target.closest("[data-output-row]").remove();
      updateRemoveButtons();
    });
    outputs.addEventListener("input", (event) => {
      if (event.target instanceof Element && event.target.matches("[data-output-name]")) updateRemoveButtons();
    });
    previewButton.addEventListener("click", () => send("/preview", "Loading preview...", renderPreview));
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      send("/feeds/split", "Publishing split feeds...", renderPublished);
    });
    updateRemoveButtons();
  })();
</script>
</body>
</html>`))

var authTemplate = template.Must(template.New("auth").Parse(`<!doctype html>
<html lang="en" class="h-full bg-slate-100 dark:bg-slate-950">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <script>
    (() => {
      try {
        const preference = localStorage.getItem("rss-studio-theme");
        const dark = preference === "dark" || (preference !== "light" && window.matchMedia("(prefers-color-scheme: dark)").matches);
        document.documentElement.classList.toggle("dark", dark);
        document.documentElement.style.colorScheme = dark ? "dark" : "light";
      } catch {}
    })();
  </script>
  <title>{{if .Registration}}Create account{{else}}Login{{end}} · RSS Studio</title>
  <script src="https://cdn.tailwindcss.com"></script>
  <script>
    tailwind.config = { darkMode: "class" };
  </script>
</head>
<body class="flex min-h-full items-center justify-center bg-slate-100 px-5 py-12 text-slate-950 antialiased dark:bg-slate-950 dark:text-slate-100">
  <main class="w-full max-w-md rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800 sm:p-8">
    <div class="mb-8">
      <div class="flex items-start justify-between gap-4">
        <a href="/" class="flex items-center gap-2 text-base font-semibold tracking-tight">
          <span class="flex h-8 w-8 items-center justify-center rounded-lg bg-violet-600 text-sm font-black text-white">R</span>
          RSS Studio
        </a>
        <div class="flex items-center gap-3">
          <a href="/api" class="text-xs font-semibold text-violet-600 hover:text-violet-700 dark:text-violet-400 dark:hover:text-violet-300">API</a>
          <button id="theme-toggle" type="button" aria-pressed="false" aria-label="Switch to dark mode" class="inline-flex items-center gap-2 rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 text-xs font-medium text-slate-600 transition hover:bg-slate-100 focus:outline-none focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-300 dark:hover:bg-slate-700">
            <span data-theme-icon aria-hidden="true">☾</span><span data-theme-label>Dark mode</span>
          </button>
        </div>
      </div>
      <h1 class="mt-8 text-2xl font-semibold tracking-tight">{{if .Registration}}Create account{{else}}Login{{end}}</h1>
      <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">{{if .Registration}}Create an account to manage your filtered feeds.{{else}}Login to manage your filtered feeds.{{end}}</p>
    </div>
    {{if .Error}}<div class="mb-6 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700 dark:border-rose-800 dark:bg-rose-950/50 dark:text-rose-300" role="alert">{{.Error}}</div>{{end}}
    <form method="post" action="{{if .Registration}}/auth/sign-up{{else}}/auth/login{{end}}" class="space-y-5">
      <div>
        <label for="username" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Username</label>
        <input id="username" name="username" autocomplete="username" required autofocus class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100">
      </div>
      <div>
        <label for="password" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Password</label>
        <input id="password" name="password" type="password" autocomplete="{{if .Registration}}new-password{{else}}current-password{{end}}" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100">
      </div>
      <button type="submit" class="inline-flex min-h-[46px] w-full items-center justify-center rounded-lg bg-violet-600 px-5 py-3 text-sm font-semibold text-white shadow-sm transition hover:bg-violet-700 focus:outline-none focus:ring-4 focus:ring-violet-600/20">{{if .Registration}}Create account{{else}}Login{{end}}</button>
    </form>
    <p class="mt-6 text-center text-sm text-slate-500 dark:text-slate-400">{{if .Registration}}Already have an account? <a class="font-semibold text-violet-600 hover:text-violet-700 dark:text-violet-400 dark:hover:text-violet-300" href="/auth/login">Login</a>{{else}}Need an account? <a class="font-semibold text-violet-600 hover:text-violet-700 dark:text-violet-400 dark:hover:text-violet-300" href="/auth/sign-up">Create one</a>{{end}}</p>
  </main>
<script>
  (() => {
    const root = document.documentElement;
    const toggle = document.getElementById("theme-toggle");
    if (!toggle) return;
    const icon = toggle.querySelector("[data-theme-icon]");
    const label = toggle.querySelector("[data-theme-label]");
    const update = () => {
      const dark = root.classList.contains("dark");
      root.style.colorScheme = dark ? "dark" : "light";
      toggle.setAttribute("aria-pressed", String(dark));
      toggle.setAttribute("aria-label", dark ? "Switch to light mode" : "Switch to dark mode");
      icon.textContent = dark ? "☀" : "☾";
      label.textContent = dark ? "Light mode" : "Dark mode";
    };
    toggle.addEventListener("click", () => {
      const dark = !root.classList.contains("dark");
      root.classList.toggle("dark", dark);
      try {
        localStorage.setItem("rss-studio-theme", dark ? "dark" : "light");
      } catch {}
      update();
    });
    update();
  })();
</script>
</body>
</html>`))
