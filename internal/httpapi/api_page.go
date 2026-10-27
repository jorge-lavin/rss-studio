package httpapi

import (
	"html/template"
	"log"
	"net/http"

	"rss-studio/internal/version"
)

type apiPageData struct {
	BaseURL string
	Version string
}

func (h *Handler) apiPage(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	if err := apiTemplate.Execute(writer, apiPageData{BaseURL: baseURL(request), Version: version.Version}); err != nil {
		log.Printf("render API page: %v", err)
	}
}

var apiTemplate = template.Must(template.New("api").Parse(`<!doctype html>
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
  <link rel="stylesheet" href="/assets/rss-studio.css">
  <title>API Reference · RSS Studio</title>
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
          <a href="/" class="text-xs font-medium text-slate-300 transition hover:text-white">Back to app</a>
          <button id="theme-toggle" type="button" aria-pressed="false" aria-label="Switch to dark mode" class="inline-flex items-center gap-2 rounded-lg border border-white/10 bg-white/5 px-3 py-2 text-xs font-medium text-slate-300 transition hover:bg-white/10 focus:outline-none focus:ring-4 focus:ring-violet-400/30">
            <span data-theme-icon aria-hidden="true">☾</span><span data-theme-label>Dark mode</span>
          </button>
        </div>
      </nav>
    </div>
  </header>

  <main class="mx-auto max-w-6xl px-5 pb-16 sm:px-8">
    <section class="mt-10 rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800 sm:p-8">
      <div class="flex flex-col gap-6 lg:flex-row lg:items-end lg:justify-between">
        <div class="max-w-2xl">
          <p class="text-xs font-semibold uppercase tracking-widest text-violet-600 dark:text-violet-400">Developer API · v1</p>
          <h1 class="mt-3 text-3xl font-semibold tracking-tight sm:text-4xl">Build with RSS Studio</h1>
          <p class="mt-3 text-base leading-7 text-slate-500 dark:text-slate-400">Create, preview, and manage filtered podcast feeds from your own tools. This reference covers the versioned JSON API without getting in your way.</p>
        </div>
        <div class="shrink-0 rounded-xl bg-slate-950 px-4 py-3 text-sm text-slate-200 dark:bg-slate-800">
          <p class="text-xs font-medium uppercase tracking-wider text-slate-400">Base URL</p>
          <code class="mt-1 block break-all text-violet-300">{{.BaseURL}}/api/v1</code>
        </div>
      </div>
      <div class="mt-8 grid gap-4 border-t border-slate-100 pt-6 text-sm dark:border-slate-800 sm:grid-cols-3">
        <div><p class="font-semibold">JSON everywhere</p><p class="mt-1 text-slate-500 dark:text-slate-400">Requests and responses use application/json.</p></div>
        <div><p class="font-semibold">Bearer auth</p><p class="mt-1 text-slate-500 dark:text-slate-400">Use a scoped service token.</p></div>
        <div><p class="font-semibold">Public RSS</p><p class="mt-1 text-slate-500 dark:text-slate-400">Published feed URLs need no API credential.</p></div>
      </div>
    </section>

    <section class="mt-8 grid gap-8 lg:grid-cols-[1.1fr_0.9fr]">
      <article class="rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800 sm:p-8">
        <div class="mb-5">
          <p class="text-xs font-semibold uppercase tracking-widest text-slate-400">Start here</p>
          <h2 class="mt-2 text-xl font-semibold tracking-tight">Authenticate</h2>
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Register once, then send the returned token with management requests.</p>
        </div>
        <pre class="overflow-x-auto rounded-xl bg-slate-950 p-4 text-xs leading-6 text-slate-300"><code>curl -fsS -X POST '{{.BaseURL}}/api/v1/auth/register' \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"change-this-password"}'

token=$(curl -fsS -X POST '{{.BaseURL}}/api/v1/auth/login' \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"change-this-password"}' \
  | jq -r .access_token)

curl -fsS '{{.BaseURL}}/api/v1/feeds' \
  -H "Authorization: Bearer $token"</code></pre>
        <p class="mt-4 text-xs leading-5 text-slate-500 dark:text-slate-400">Service tokens can be created with explicit scopes for automation. Raw service-token values are shown only once.</p>
      </article>

      <article class="rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800 sm:p-8">
        <div class="mb-5">
          <p class="text-xs font-semibold uppercase tracking-widest text-slate-400">Common payload</p>
          <h2 class="mt-2 text-xl font-semibold tracking-tight">Create a feed</h2>
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Operators are case-insensitive and default to contains.</p>
        </div>
        <pre class="overflow-x-auto rounded-xl bg-slate-950 p-4 text-xs leading-6 text-slate-300"><code>curl -fsS -X POST '{{.BaseURL}}/api/v1/feeds' \
  -H "Authorization: Bearer $token" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "AI episodes",
    "source_url": "https://example.com/podcast.xml",
    "operator": "contains",
    "keyword": "AI"
  }'</code></pre>
        <div class="mt-4 flex flex-wrap gap-2 text-xs">
          <code class="rounded-full bg-violet-50 px-2.5 py-1 text-violet-700 dark:bg-violet-950 dark:text-violet-300">contains</code>
          <code class="rounded-full bg-violet-50 px-2.5 py-1 text-violet-700 dark:bg-violet-950 dark:text-violet-300">starts_with</code>
          <code class="rounded-full bg-violet-50 px-2.5 py-1 text-violet-700 dark:bg-violet-950 dark:text-violet-300">ends_with</code>
          <code class="rounded-full bg-violet-50 px-2.5 py-1 text-violet-700 dark:bg-violet-950 dark:text-violet-300">equals</code>
        </div>
      </article>
    </section>

    <section class="mt-8" aria-labelledby="endpoints-heading">
      <div class="mb-5 flex items-end justify-between gap-4">
        <div>
          <p class="text-xs font-semibold uppercase tracking-widest text-slate-400">Reference</p>
          <h2 id="endpoints-heading" class="mt-2 text-2xl font-semibold tracking-tight">Endpoints</h2>
        </div>
        <span class="rounded-full bg-slate-200 px-3 py-1 text-xs font-medium text-slate-600 dark:bg-slate-800 dark:text-slate-300">API v1</span>
      </div>

      <div class="space-y-4">
        <article class="overflow-hidden rounded-2xl bg-white shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800">
          <div class="border-b border-slate-100 px-5 py-4 dark:border-slate-800 sm:px-6"><h3 class="font-semibold">Authentication</h3><p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Public endpoints for account access.</p></div>
          <div class="divide-y divide-slate-100 dark:divide-slate-800">
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-violet-50 px-2 py-1 text-center text-xs font-bold text-violet-700 dark:bg-violet-950 dark:text-violet-300">POST</span><code class="text-sm font-semibold">/auth/register</code><span class="text-sm text-slate-500 dark:text-slate-400">Create an account.</span></div>
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-violet-50 px-2 py-1 text-center text-xs font-bold text-violet-700 dark:bg-violet-950 dark:text-violet-300">POST</span><code class="text-sm font-semibold">/auth/login</code><span class="text-sm text-slate-500 dark:text-slate-400">Issue a scoped service token.</span></div>
          </div>
        </article>

        <article class="overflow-hidden rounded-2xl bg-white shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800">
          <div class="border-b border-slate-100 px-5 py-4 dark:border-slate-800 sm:px-6"><h3 class="font-semibold">Service tokens</h3><p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Requires a service token with <code>tokens:manage</code>.</p></div>
          <div class="divide-y divide-slate-100 dark:divide-slate-800">
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-sky-50 px-2 py-1 text-center text-xs font-bold text-sky-700 dark:bg-sky-950 dark:text-sky-300">GET</span><code class="text-sm font-semibold">/auth/tokens</code><span class="text-sm text-slate-500 dark:text-slate-400">List token metadata.</span></div>
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-violet-50 px-2 py-1 text-center text-xs font-bold text-violet-700 dark:bg-violet-950 dark:text-violet-300">POST</span><code class="text-sm font-semibold">/auth/tokens</code><span class="text-sm text-slate-500 dark:text-slate-400">Create a token with scopes and expiry.</span></div>
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-violet-50 px-2 py-1 text-center text-xs font-bold text-violet-700 dark:bg-violet-950 dark:text-violet-300">POST</span><code class="text-sm font-semibold">/auth/tokens/{id}/revoke</code><span class="text-sm text-slate-500 dark:text-slate-400">Revoke a token.</span></div>
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-rose-50 px-2 py-1 text-center text-xs font-bold text-rose-700 dark:bg-rose-950 dark:text-rose-300">DELETE</span><code class="text-sm font-semibold">/auth/tokens/{id}</code><span class="text-sm text-slate-500 dark:text-slate-400">Revoke a token.</span></div>
          </div>
        </article>

        <article class="overflow-hidden rounded-2xl bg-white shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800">
          <div class="border-b border-slate-100 px-5 py-4 dark:border-slate-800 sm:px-6"><h3 class="font-semibold">Feeds</h3><p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Manage feeds owned by authenticated account.</p></div>
          <div class="divide-y divide-slate-100 dark:divide-slate-800">
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-sky-50 px-2 py-1 text-center text-xs font-bold text-sky-700 dark:bg-sky-950 dark:text-sky-300">GET</span><code class="text-sm font-semibold">/feeds</code><span class="text-sm text-slate-500 dark:text-slate-400">List feeds. Requires <code>feeds:read</code>.</span></div>
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-violet-50 px-2 py-1 text-center text-xs font-bold text-violet-700 dark:bg-violet-950 dark:text-violet-300">POST</span><code class="text-sm font-semibold">/feeds</code><span class="text-sm text-slate-500 dark:text-slate-400">Publish one filtered feed. Requires <code>feeds:write</code>.</span></div>
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-violet-50 px-2 py-1 text-center text-xs font-bold text-violet-700 dark:bg-violet-950 dark:text-violet-300">POST</span><code class="text-sm font-semibold">/feeds/preview</code><span class="text-sm text-slate-500 dark:text-slate-400">Preview matches, episode artwork, and podcast artwork without publishing. Requires <code>feeds:preview</code>.</span></div>
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-violet-50 px-2 py-1 text-center text-xs font-bold text-violet-700 dark:bg-violet-950 dark:text-violet-300">POST</span><code class="text-sm font-semibold">/feeds/split</code><span class="text-sm text-slate-500 dark:text-slate-400">Publish up to 32 filtered outputs. Requires <code>feeds:write</code>.</span></div>
            <div class="flex flex-col gap-2 px-5 py-4 sm:flex-row sm:items-center sm:gap-4 sm:px-6"><span class="w-16 rounded-md bg-rose-50 px-2 py-1 text-center text-xs font-bold text-rose-700 dark:bg-rose-950 dark:text-rose-300">DELETE</span><code class="text-sm font-semibold">/feeds/{path}</code><span class="text-sm text-slate-500 dark:text-slate-400">Delete a feed. Requires <code>feeds:delete</code>.</span></div>
          </div>
        </article>
      </div>
    </section>

    <section class="mt-8 grid gap-8 lg:grid-cols-2">
      <article class="rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800 sm:p-8">
        <p class="text-xs font-semibold uppercase tracking-widest text-slate-400">Response shape</p>
        <h2 class="mt-2 text-xl font-semibold tracking-tight">Predictable errors</h2>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Errors keep a stable machine-readable code and request ID.</p>
        <pre class="mt-5 overflow-x-auto rounded-xl bg-slate-950 p-4 text-xs leading-6 text-slate-300"><code>{
  "error": {
    "code": "validation_error",
    "message": "Request body is invalid."
  },
  "request_id": "..."
}</code></pre>
        <p class="mt-4 text-xs leading-5 text-slate-500 dark:text-slate-400">The same request ID is returned in <code>X-Request-ID</code>. Rate limits and quotas include retry metadata when applicable.</p>
      </article>
      <article class="rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800 sm:p-8">
        <p class="text-xs font-semibold uppercase tracking-widest text-slate-400">Keep in mind</p>
        <h2 class="mt-2 text-xl font-semibold tracking-tight">Safe integrations</h2>
        <ul class="mt-5 space-y-3 text-sm leading-6 text-slate-500 dark:text-slate-400">
          <li class="flex gap-3"><span class="font-bold text-violet-600 dark:text-violet-400">01</span><span>Use <code>Idempotency-Key</code> when retrying feed creation or split requests.</span></li>
          <li class="flex gap-3"><span class="font-bold text-violet-600 dark:text-violet-400">02</span><span>Keep service tokens in a secret store. They are never returned by list operations.</span></li>
          <li class="flex gap-3"><span class="font-bold text-violet-600 dark:text-violet-400">03</span><span>Published <code>/feeds/*.xml</code> URLs are public and can be added to any RSS reader.</span></li>
        </ul>
      </article>
    </section>
  </main>

  <footer class="mx-auto max-w-6xl px-5 pb-8 text-xs text-slate-400 sm:px-8">RSS Studio · API v1 · Version {{.Version}}</footer>
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
