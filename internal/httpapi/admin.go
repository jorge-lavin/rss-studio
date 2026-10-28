package httpapi

import (
	"errors"
	"html/template"
	"log"
	"net/http"

	"rss-studio/internal/domain"
	"rss-studio/internal/version"
)

type adminUsersPageData struct {
	Users   []domain.UserSummary
	Version string
	Error   string
}

func (h *Handler) adminUsersRedirect(writer http.ResponseWriter, request *http.Request) {
	http.Redirect(writer, request, "/admin/users", http.StatusSeeOther)
}

func (h *Handler) adminUsers(writer http.ResponseWriter, _ *http.Request) {
	users, err := h.auth.ListUsers()
	status := http.StatusOK
	message := ""
	if err != nil {
		log.Printf("list users: %v", err)
		users = nil
		status = http.StatusInternalServerError
		if errors.Is(err, ErrUserListingUnavailable) {
			status = http.StatusNotImplemented
			message = "User listing is unavailable."
		} else {
			message = "Could not read users."
		}
	}
	noStore(writer)
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	if err := adminUsersTemplate.Execute(writer, adminUsersPageData{Users: users, Version: version.Version, Error: message}); err != nil {
		log.Printf("render admin users: %v", err)
	}
}

var adminUsersTemplate = template.Must(template.New("admin-users").Parse(`<!doctype html>
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
  <title>Admin · RSS Studio</title>
</head>
<body class="min-h-full bg-slate-100 text-slate-950 antialiased dark:bg-slate-950 dark:text-slate-100">
<div class="min-h-screen">
  <header class="bg-slate-950 text-white">
    <div class="mx-auto max-w-6xl px-5 sm:px-8">
      <nav class="flex items-center justify-between border-b border-white/10 py-5">
        <a href="/" class="flex items-center gap-2 text-base font-semibold tracking-tight"><span class="flex h-8 w-8 items-center justify-center rounded-lg bg-violet-600 text-sm font-black text-white">R</span>RSS Studio</a>
        <a href="/" class="text-xs font-medium text-slate-300 transition hover:text-white">Back to app</a>
      </nav>
    </div>
  </header>
  <main class="mx-auto max-w-6xl px-5 pb-16 sm:px-8">
    <section class="mt-10 rounded-2xl bg-white p-6 shadow-xl shadow-slate-900/5 ring-1 ring-slate-200 dark:bg-slate-900 dark:ring-slate-800 sm:p-8">
      <div class="flex items-end justify-between gap-4">
        <div><p class="text-xs font-semibold uppercase tracking-widest text-violet-600 dark:text-violet-400">Administration</p><h1 class="mt-3 text-3xl font-semibold tracking-tight">Users</h1><p class="mt-3 text-sm text-slate-500 dark:text-slate-400">Current accounts in RSS Studio.</p></div>
        <span class="rounded-full bg-slate-200 px-3 py-1 text-sm font-medium text-slate-600 dark:bg-slate-800 dark:text-slate-300">{{len .Users}}</span>
      </div>
      {{if .Error}}<div class="mt-6 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700 dark:border-rose-800 dark:bg-rose-950/50 dark:text-rose-300" role="alert">{{.Error}}</div>{{end}}
      <div class="mt-8 overflow-hidden rounded-xl border border-slate-200 dark:border-slate-800">
        <table class="w-full text-left text-sm"><thead class="bg-slate-50 text-xs uppercase tracking-wider text-slate-500 dark:bg-slate-800/50 dark:text-slate-400"><tr><th class="px-4 py-3 font-semibold">Username</th><th class="px-4 py-3 font-semibold">Created</th></tr></thead><tbody class="divide-y divide-slate-200 dark:divide-slate-800">{{range .Users}}<tr><td class="px-4 py-4 font-medium">{{.Username}}</td><td class="px-4 py-4 text-slate-500 dark:text-slate-400">{{.CreatedAt.Format "2006-01-02 15:04 UTC"}}</td></tr>{{else}}<tr><td colspan="2" class="px-4 py-8 text-center text-slate-500 dark:text-slate-400">No users found.</td></tr>{{end}}</tbody></table>
      </div>
    </section>
  </main>
  <footer class="mx-auto max-w-6xl px-5 pb-8 text-xs text-slate-400 sm:px-8">RSS Studio · Version {{.Version}}</footer>
</div>
</body>
</html>`))
