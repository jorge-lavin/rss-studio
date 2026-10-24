package httpapi

import (
	"context"
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
	"rss-studio/internal/observability"
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

type authPageData struct {
	Error        string
	Registration bool
}

type authCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type authenticatedUsernameContextKey struct{}

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
		mux.HandleFunc("GET /auth/sign-up", handler.registerPage)
		mux.HandleFunc("GET /auth/register", handler.registerPage)
		mux.HandleFunc("POST /auth/sign-up", handler.register)
		mux.HandleFunc("POST /auth/register", handler.register)
	}
	mux.HandleFunc("POST /feeds", handler.createFeed)
	mux.HandleFunc("GET /feeds/{file}", handler.serveFeed)
	mux.HandleFunc("POST /feeds/{slug}/delete", handler.deleteFeed)
	return observability.HTTPHandler(handler.authenticate(mux))
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
	http.Redirect(writer, request, "/auth/sign-in", http.StatusSeeOther)
}

func parseCredentials(request *http.Request) (authCredentials, error) {
	var credentials authCredentials
	if isJSONRequest(request) {
		if err := json.NewDecoder(request.Body).Decode(&credentials); err != nil {
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
		username, err := h.auth.Subject(request)
		if err != nil {
			if request.Method == http.MethodGet && acceptsHTML(request) {
				http.Redirect(writer, request, "/auth/sign-in", http.StatusSeeOther)
				return
			}
			writer.Header().Set("WWW-Authenticate", `Bearer realm="rss-studio"`)
			http.Error(writer, "Authentication required.", http.StatusUnauthorized)
			return
		}
		request = request.WithContext(context.WithValue(request.Context(), authenticatedUsernameContextKey{}, username))
		next.ServeHTTP(writer, request)
	})
}

func acceptsHTML(request *http.Request) bool {
	return strings.Contains(strings.ToLower(request.Header.Get("Accept")), "text/html")
}

func isPublicRequest(request *http.Request) bool {
	if request.URL.Path == "/auth/login" || request.URL.Path == "/auth/sign-in" || request.URL.Path == "/auth/register" || request.URL.Path == "/auth/sign-up" {
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
	_, err := h.service.CreateFeed(request.Context(), authenticatedUsername(request), request.FormValue("name"), request.FormValue("source_url"), request.FormValue("keyword"))
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
	if err := h.service.DeleteFeed(authenticatedUsername(request), request.PathValue("slug")); err != nil {
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
	if err := homeTemplate.Execute(writer, homePageData{Feeds: h.service.ListFeeds(authenticatedUsername(request)), BaseURL: baseURL(request), Version: version.Version, Error: message}); err != nil {
		log.Printf("render home: %v", err)
	}
}

func authenticatedUsername(request *http.Request) string {
	username, _ := request.Context().Value(authenticatedUsernameContextKey{}).(string)
	return username
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
  <script>
    window.tailwind = window.tailwind || {};
    window.tailwind.config = { darkMode: "class" };
  </script>
  <title>RSS Studio</title>
  <script src="https://cdn.tailwindcss.com"></script>
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
          <span class="hidden text-sm text-slate-400 sm:block">Podcast feed filter</span>
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
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">One keyword. Less noise.</p>
        </div>
        <span class="text-xs font-medium uppercase tracking-wider text-slate-400">New feed</span>
      </div>
      {{if .Error}}<div class="mb-6 flex items-start gap-3 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700 dark:border-rose-800 dark:bg-rose-950/50 dark:text-rose-300" role="alert"><span class="mt-0.5 font-bold">!</span><p>{{.Error}}</p></div>{{end}}
      <form method="post" action="/feeds" class="grid gap-5 lg:grid-cols-[1fr_1.4fr_0.8fr_auto] lg:items-end">
        <div>
          <label for="name" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Feed name</label>
          <input id="name" name="name" maxlength="120" placeholder="My tech episodes" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
        </div>
        <div>
          <label for="source_url" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Source RSS URL</label>
          <input id="source_url" name="source_url" type="url" maxlength="2048" placeholder="https://example.com/feed.xml" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
        </div>
        <div>
          <label for="keyword" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Match title</label>
          <input id="keyword" name="keyword" maxlength="200" placeholder="e.g. AI" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-500">
        </div>
        <button type="submit" class="inline-flex min-h-[46px] items-center justify-center gap-2 rounded-lg bg-violet-600 px-5 py-3 text-sm font-semibold text-white shadow-sm transition hover:bg-violet-700 focus:outline-none focus:ring-4 focus:ring-violet-600/20 lg:mb-0">Publish feed <span aria-hidden="true">→</span></button>
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
                <span class="rounded-full bg-violet-50 px-2.5 py-1 text-xs font-medium text-violet-700 dark:bg-violet-950 dark:text-violet-300">Contains “{{.Keyword}}”</span>
              </div>
              <p class="mt-2 truncate text-sm text-slate-500 dark:text-slate-400" title="{{.SourceURL}}">Source: {{.SourceURL}}</p>
              <a href="/feeds/{{.Slug}}.xml" class="mt-3 block truncate text-sm font-medium text-violet-600 hover:text-violet-700 hover:underline dark:text-violet-400 dark:hover:text-violet-300" title="{{$.BaseURL}}/feeds/{{.Slug}}.xml">{{$.BaseURL}}/feeds/{{.Slug}}.xml</a>
            </div>
            <div class="flex shrink-0 items-center gap-4 border-t border-slate-100 pt-4 dark:border-slate-800 lg:border-0 lg:pt-0">
              <a href="/feeds/{{.Slug}}.xml" class="text-sm font-semibold text-slate-700 hover:text-slate-950 dark:text-slate-300 dark:hover:text-slate-100">Open feed <span aria-hidden="true">↗</span></a>
              <form method="post" action="/feeds/{{.Slug}}/delete"><button class="text-sm font-medium text-rose-600 hover:text-rose-700 dark:text-rose-400 dark:hover:text-rose-300" type="submit">Delete</button></form>
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
  <script>
    window.tailwind = window.tailwind || {};
    window.tailwind.config = { darkMode: "class" };
  </script>
  <title>{{if .Registration}}Create account{{else}}Sign in{{end}} · RSS Studio</title>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="flex min-h-full items-center justify-center bg-slate-100 px-5 py-12 text-slate-950 antialiased dark:bg-slate-950 dark:text-slate-100">
  <main class="w-full max-w-md rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800 sm:p-8">
    <div class="mb-8">
      <div class="flex items-start justify-between gap-4">
        <a href="/" class="flex items-center gap-2 text-base font-semibold tracking-tight">
          <span class="flex h-8 w-8 items-center justify-center rounded-lg bg-violet-600 text-sm font-black text-white">R</span>
          RSS Studio
        </a>
        <button id="theme-toggle" type="button" aria-pressed="false" aria-label="Switch to dark mode" class="inline-flex items-center gap-2 rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 text-xs font-medium text-slate-600 transition hover:bg-slate-100 focus:outline-none focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-300 dark:hover:bg-slate-700">
          <span data-theme-icon aria-hidden="true">☾</span><span data-theme-label>Dark mode</span>
        </button>
      </div>
      <h1 class="mt-8 text-2xl font-semibold tracking-tight">{{if .Registration}}Create account{{else}}Sign in{{end}}</h1>
      <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">{{if .Registration}}Create an account to manage your filtered feeds.{{else}}Sign in to manage your filtered feeds.{{end}}</p>
    </div>
    {{if .Error}}<div class="mb-6 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700 dark:border-rose-800 dark:bg-rose-950/50 dark:text-rose-300" role="alert">{{.Error}}</div>{{end}}
    <form method="post" action="{{if .Registration}}/auth/sign-up{{else}}/auth/sign-in{{end}}" class="space-y-5">
      <div>
        <label for="username" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Username</label>
        <input id="username" name="username" autocomplete="username" required autofocus class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100">
      </div>
      <div>
        <label for="password" class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Password</label>
        <input id="password" name="password" type="password" autocomplete="{{if .Registration}}new-password{{else}}current-password{{end}}" required class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-3 text-sm text-slate-900 shadow-sm outline-none transition focus:border-violet-600 focus:ring-4 focus:ring-violet-600/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100">
      </div>
      <button type="submit" class="inline-flex min-h-[46px] w-full items-center justify-center rounded-lg bg-violet-600 px-5 py-3 text-sm font-semibold text-white shadow-sm transition hover:bg-violet-700 focus:outline-none focus:ring-4 focus:ring-violet-600/20">{{if .Registration}}Create account{{else}}Sign in{{end}}</button>
</form>
    <p class="mt-6 text-center text-sm text-slate-500 dark:text-slate-400">{{if .Registration}}Already have an account? <a class="font-semibold text-violet-600 hover:text-violet-700 dark:text-violet-400 dark:hover:text-violet-300" href="/auth/sign-in">Sign in</a>{{else}}Need an account? <a class="font-semibold text-violet-600 hover:text-violet-700 dark:text-violet-400 dark:hover:text-violet-300" href="/auth/sign-up">Create one</a>{{end}}</p>
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
