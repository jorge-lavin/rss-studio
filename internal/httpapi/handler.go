package httpapi

import (
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/domain"
	"rss-studio/internal/version"
)

type Handler struct {
	service      *application.Service
	auth         *Authenticator
	loginLimiter *loginRateLimiter
}

type homePageData struct {
	Feeds   []domain.Feed
	BaseURL string
	Version string
	Error   string
}

func NewHandler(service *application.Service, authenticators ...*Authenticator) http.Handler {
	var auth *Authenticator
	if len(authenticators) > 0 {
		auth = authenticators[0]
	}
	handler := &Handler{service: service, auth: auth, loginLimiter: newLoginRateLimiter()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handler.home)
	if auth != nil {
		mux.HandleFunc("GET /auth/sign-in", handler.loginPage)
		mux.HandleFunc("GET /auth/login", handler.loginPage)
		mux.HandleFunc("POST /auth/sign-in", handler.login)
		mux.HandleFunc("POST /auth/login", handler.login)
	}
	mux.HandleFunc("POST /feeds", handler.createFeed)
	mux.HandleFunc("GET /feeds/{file}", handler.serveFeed)
	mux.HandleFunc("POST /feeds/{slug}/delete", handler.deleteFeed)
	return requestLogger(handler.authenticate(mux))
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

	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if isJSONRequest(request) {
		if err := json.NewDecoder(request.Body).Decode(&credentials); err != nil {
			http.Error(writer, "Invalid login request.", http.StatusBadRequest)
			return
		}
	} else if err := request.ParseForm(); err != nil {
		http.Error(writer, "Invalid login request.", http.StatusBadRequest)
		return
	} else {
		credentials.Username = request.FormValue("username")
		credentials.Password = request.FormValue("password")
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

func (h *Handler) loginPage(writer http.ResponseWriter, request *http.Request) {
	h.renderLogin(writer, "", http.StatusOK)
}

func (h *Handler) renderLogin(writer http.ResponseWriter, message string, status int) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	if err := loginTemplate.Execute(writer, struct{ Error string }{Error: message}); err != nil {
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
		if err := h.auth.ValidateRequest(request); err != nil {
			if request.Method == http.MethodGet && acceptsHTML(request) {
				http.Redirect(writer, request, "/auth/sign-in", http.StatusSeeOther)
				return
			}
			writer.Header().Set("WWW-Authenticate", `Bearer realm="rss-studio"`)
			http.Error(writer, "Authentication required.", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func acceptsHTML(request *http.Request) bool {
	return strings.Contains(strings.ToLower(request.Header.Get("Accept")), "text/html")
}

func isPublicRequest(request *http.Request) bool {
	if request.URL.Path == "/auth/login" || request.URL.Path == "/auth/sign-in" {
		return request.Method == http.MethodGet || request.Method == http.MethodPost
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
	_, err := h.service.CreateFeed(request.Context(), request.FormValue("name"), request.FormValue("source_url"), request.FormValue("keyword"))
	if err != nil {
		switch {
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

func (h *Handler) serveFeed(writer http.ResponseWriter, request *http.Request) {
	file := request.PathValue("file")
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
			log.Printf("serve feed: %v", err)
			http.Error(writer, "Could not read feed.", http.StatusInternalServerError)
		}
		return
	}
	writer.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	writer.Header().Set("Cache-Control", "public, max-age=300")
	writer.Write(data)
}

func (h *Handler) deleteFeed(writer http.ResponseWriter, request *http.Request) {
	if err := h.service.DeleteFeed(request.PathValue("slug")); err != nil {
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

func (h *Handler) renderHome(writer http.ResponseWriter, request *http.Request, message string, status int) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	if err := homeTemplate.Execute(writer, homePageData{Feeds: h.service.ListFeeds(), BaseURL: baseURL(request), Version: version.Version, Error: message}); err != nil {
		log.Printf("render home: %v", err)
	}
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

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(writer, request)
		log.Printf("%s %s %s", request.Method, request.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}

var homeTemplate = template.Must(template.New("home").Parse(`<!doctype html>
<html lang="en" class="h-full bg-slate-100">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>RSS Studio</title>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-full bg-slate-100 text-slate-950 antialiased">
<div class="min-h-screen">
  <header class="bg-slate-950 text-white">
    <div class="mx-auto max-w-6xl px-5 sm:px-8">
      <nav class="flex items-center justify-between border-b border-white/10 py-5">
        <a href="/" class="flex items-center gap-2 text-base font-semibold tracking-tight">
          <span class="flex h-8 w-8 items-center justify-center rounded-lg bg-violet-600 text-sm font-black text-white">R</span>
          RSS Studio
        </a>
        <span class="hidden text-sm text-slate-400 sm:block">Podcast feed filter</span>
      </nav>
    </div>
  </header>

  <main class="mx-auto max-w-6xl px-5 pb-16 sm:px-8">
    <section class="mt-10 rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 sm:p-8">
      <div class="mb-7 flex flex-col gap-2 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h2 class="text-xl font-semibold tracking-tight">Create a filtered feed</h2>
          <p class="mt-1 text-sm text-slate-500">One keyword. Less noise.</p>
        </div>
        <span class="text-xs font-medium uppercase tracking-wider text-slate-400">New feed</span>
      </div>
      {{if .Error}}<div class="mb-6 flex items-start gap-3 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700" role="alert"><span class="mt-0.5 font-bold">!</span><p>{{.Error}}</p></div>{{end}}
      <form method="post" action="/feeds" class="grid gap-5 lg:grid-cols-[1fr_1.4fr_0.8fr_auto] lg:items-end">
        <div>
          <label for="name" class="mb-2 block text-sm font-medium text-slate-700">Feed name</label>
          <input id="name" name="name" maxlength="120" placeholder="My tech episodes" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10">
        </div>
        <div>
          <label for="source_url" class="mb-2 block text-sm font-medium text-slate-700">Source RSS URL</label>
          <input id="source_url" name="source_url" type="url" maxlength="2048" placeholder="https://example.com/feed.xml" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10">
        </div>
        <div>
          <label for="keyword" class="mb-2 block text-sm font-medium text-slate-700">Match title</label>
          <input id="keyword" name="keyword" maxlength="200" placeholder="e.g. AI" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10">
        </div>
        <button type="submit" class="inline-flex min-h-[46px] items-center justify-center gap-2 rounded-lg bg-violet-600 px-5 py-3 text-sm font-semibold text-white shadow-sm transition hover:bg-violet-700 focus:outline-none focus:ring-4 focus:ring-violet-600/20 lg:mb-0">Publish feed <span aria-hidden="true">→</span></button>
      </form>
    </section>

    <section class="mt-12">
      <div class="mb-5 flex items-center justify-between gap-4">
        <div>
          <h2 class="text-xl font-semibold tracking-tight">Published feeds</h2>
          <p class="mt-1 text-sm text-slate-500">Ready to add to any RSS reader.</p>
        </div>
        <span class="rounded-full bg-slate-200 px-3 py-1 text-sm font-medium text-slate-600">{{len .Feeds}}</span>
      </div>
      {{if not .Feeds}}
      <div class="rounded-2xl border border-dashed border-slate-300 bg-white px-6 py-14 text-center">
        <div class="mx-auto flex h-11 w-11 items-center justify-center rounded-full bg-violet-50 text-xl text-violet-600">＋</div>
        <h3 class="mt-4 text-sm font-semibold text-slate-900">No filtered feeds yet</h3>
        <p class="mt-1 text-sm text-slate-500">Create your first one above.</p>
      </div>
      {{else}}
      <div class="space-y-3">
        {{range .Feeds}}
        <article class="rounded-2xl border border-slate-200 bg-white p-5 shadow-sm sm:p-6">
          <div class="flex flex-col gap-5 lg:flex-row lg:items-center lg:justify-between">
            <div class="min-w-0">
              <div class="flex flex-wrap items-center gap-3">
                <h3 class="text-base font-semibold text-slate-900">{{.Name}}</h3>
                <span class="rounded-full bg-violet-50 px-2.5 py-1 text-xs font-medium text-violet-700">Contains “{{.Keyword}}”</span>
              </div>
              <p class="mt-2 truncate text-sm text-slate-500" title="{{.SourceURL}}">Source: {{.SourceURL}}</p>
              <a href="/feeds/{{.Slug}}.xml" class="mt-3 block truncate text-sm font-medium text-violet-600 hover:text-violet-700 hover:underline" title="{{$.BaseURL}}/feeds/{{.Slug}}.xml">{{$.BaseURL}}/feeds/{{.Slug}}.xml</a>
            </div>
            <div class="flex shrink-0 items-center gap-4 border-t border-slate-100 pt-4 lg:border-0 lg:pt-0">
              <a href="/feeds/{{.Slug}}.xml" class="text-sm font-semibold text-slate-700 hover:text-slate-950">Open feed <span aria-hidden="true">↗</span></a>
              <form method="post" action="/feeds/{{.Slug}}/delete"><button class="text-sm font-medium text-rose-600 hover:text-rose-700" type="submit">Delete</button></form>
            </div>
          </div>
        </article>
        {{end}}
      </div>
      {{end}}
    </section>
  </main>

  <footer class="mx-auto max-w-6xl px-5 pb-8 text-xs text-slate-400 sm:px-8">RSS Studio · Version {{.Version}} · Filter the feed, not your attention.</footer>
</div>
</body>
</html>`))

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en" class="h-full bg-slate-100">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Sign in · RSS Studio</title>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="flex min-h-full items-center justify-center bg-slate-100 px-5 py-12 text-slate-950 antialiased">
  <main class="w-full max-w-md rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 sm:p-8">
    <div class="mb-8">
      <a href="/" class="flex items-center gap-2 text-base font-semibold tracking-tight">
        <span class="flex h-8 w-8 items-center justify-center rounded-lg bg-violet-600 text-sm font-black text-white">R</span>
        RSS Studio
      </a>
      <h1 class="mt-8 text-2xl font-semibold tracking-tight">Sign in</h1>
      <p class="mt-1 text-sm text-slate-500">Sign in to manage your filtered feeds.</p>
      <p class="mt-2 text-sm text-slate-500">Use the account configured for this RSS Studio instance. Self-registration is not available.</p>
    </div>
    {{if .Error}}<div class="mb-6 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700" role="alert">{{.Error}}</div>{{end}}
    <form method="post" action="/auth/sign-in" class="space-y-5">
      <div>
        <label for="username" class="mb-2 block text-sm font-medium text-slate-700">Username</label>
        <input id="username" name="username" autocomplete="username" required autofocus class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10">
      </div>
      <div>
        <label for="password" class="mb-2 block text-sm font-medium text-slate-700">Password</label>
        <input id="password" name="password" type="password" autocomplete="current-password" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10">
      </div>
      <button type="submit" class="inline-flex min-h-[46px] w-full items-center justify-center rounded-lg bg-violet-600 px-5 py-3 text-sm font-semibold text-white shadow-sm transition hover:bg-violet-700 focus:outline-none focus:ring-4 focus:ring-violet-600/20">Sign in</button>
    </form>
  </main>
</body>
</html>`))
