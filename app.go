package main

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type app struct {
	store      *store
	client     *http.Client
	cacheDir   string
	refreshMu  sync.Mutex
	stateMu    sync.Mutex
	refreshing map[string]bool
	checkedAt  map[string]time.Time
}

type homePageData struct {
	Feeds   []feedDefinition
	BaseURL string
	Error   string
}

func newApp(store *store, client *http.Client) *app {
	return &app{
		store:      store,
		client:     client,
		cacheDir:   filepath.Join(filepath.Dir(store.path), "cache"),
		refreshing: make(map[string]bool),
		checkedAt:  make(map[string]time.Time),
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("POST /feeds", a.createFeed)
	mux.HandleFunc("GET /feeds/{file}", a.serveFeed)
	mux.HandleFunc("POST /feeds/{slug}/delete", a.deleteFeed)
	return requestLogger(mux)
}

func (a *app) home(writer http.ResponseWriter, request *http.Request) {
	a.renderHome(writer, request, "", http.StatusOK)
}

func (a *app) createFeed(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		a.renderHome(writer, request, "Could not read form.", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(request.FormValue("name"))
	sourceURL := strings.TrimSpace(request.FormValue("source_url"))
	keyword := strings.TrimSpace(request.FormValue("keyword"))
	if name == "" || sourceURL == "" || keyword == "" {
		a.renderHome(writer, request, "Name, source URL, and keyword are required.", http.StatusBadRequest)
		return
	}
	if len(name) > 120 || len(sourceURL) > 2048 || len(keyword) > 200 {
		a.renderHome(writer, request, "One or more fields are too long.", http.StatusBadRequest)
		return
	}
	if err := validateSourceURL(sourceURL); err != nil {
		a.renderHome(writer, request, err.Error(), http.StatusBadRequest)
		return
	}

	feed, err := a.store.create(name, sourceURL, keyword)
	if err != nil {
		log.Printf("create feed: %v", err)
		a.renderHome(writer, request, "Could not save feed.", http.StatusInternalServerError)
		return
	}
	if err := a.refreshFeed(request.Context(), feed, true); err != nil {
		log.Printf("precompute %s: %v", feed.Slug, err)
		if deleteErr := a.store.delete(feed.Slug); deleteErr != nil {
			log.Printf("roll back %s: %v", feed.Slug, deleteErr)
		}
		a.removeCachedFeed(feed.Slug)
		a.renderHome(writer, request, "Source feed could not be fetched or processed.", http.StatusBadGateway)
		return
	}
	http.Redirect(writer, request, "/", http.StatusSeeOther)
}

func (a *app) serveFeed(writer http.ResponseWriter, request *http.Request) {
	file := request.PathValue("file")
	if !strings.HasSuffix(file, ".xml") {
		http.NotFound(writer, request)
		return
	}
	slug := strings.TrimSuffix(file, ".xml")
	feed, found := a.store.get(slug)
	if !found {
		http.NotFound(writer, request)
		return
	}

	filtered, err := os.ReadFile(a.feedCachePath(feed.Slug))
	if err != nil {
		log.Printf("read cached feed %s: %v", feed.Slug, err)
		a.triggerRefresh(feed)
		http.Error(writer, "Feed cache is not ready yet.", http.StatusServiceUnavailable)
		return
	}

	writer.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	writer.Header().Set("Cache-Control", "public, max-age=300")
	writer.Write(filtered)
	a.triggerRefresh(feed)
}

func (a *app) deleteFeed(writer http.ResponseWriter, request *http.Request) {
	if err := a.store.delete(request.PathValue("slug")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(writer, request)
			return
		}
		http.Error(writer, "Could not delete feed.", http.StatusInternalServerError)
		return
	}
	a.removeCachedFeed(request.PathValue("slug"))
	http.Redirect(writer, request, "/", http.StatusSeeOther)
}

func (a *app) renderHome(writer http.ResponseWriter, request *http.Request, message string, status int) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	err := homeTemplate.Execute(writer, homePageData{
		Feeds:   a.store.list(),
		BaseURL: baseURL(request),
		Error:   message,
	})
	if err != nil {
		log.Printf("render home: %v", err)
	}
}

func validateSourceURL(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("Source URL must use http or https.")
	}
	return nil
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
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>RSS Studio</title>
  <style>
    :root { color-scheme: dark; --ink:#f4f1e8; --muted:#a9a69d; --panel:#181a19; --line:#343734; --acid:#c8f135; --danger:#ff806f; }
    * { box-sizing:border-box; }
    body { margin:0; background:#0c0e0d; color:var(--ink); font:16px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace; }
    main { width:min(1080px,calc(100% - 32px)); margin:0 auto; padding:64px 0 96px; }
    header { border-bottom:1px solid var(--line); margin-bottom:40px; padding-bottom:24px; }
    h1 { font-size:clamp(2.5rem,8vw,6rem); letter-spacing:-.08em; line-height:.85; margin:0; text-transform:uppercase; }
    h1 span { color:var(--acid); }
    header p { color:var(--muted); margin:24px 0 0; max-width:620px; }
    .grid { display:grid; grid-template-columns:minmax(280px,.8fr) minmax(360px,1.2fr); gap:24px; align-items:start; }
    section { background:var(--panel); border:1px solid var(--line); padding:24px; }
    h2 { font-size:.8rem; letter-spacing:.16em; margin:0 0 24px; text-transform:uppercase; color:var(--acid); }
    label { display:block; font-size:.78rem; color:var(--muted); margin:18px 0 6px; text-transform:uppercase; }
    input { width:100%; border:1px solid var(--line); background:#0f1110; color:var(--ink); padding:12px; border-radius:0; font:inherit; }
    input:focus { outline:2px solid var(--acid); outline-offset:1px; }
    button { border:0; background:var(--acid); color:#10120d; cursor:pointer; font:700 .8rem ui-monospace,monospace; letter-spacing:.08em; padding:13px 18px; text-transform:uppercase; }
    form > button { margin-top:24px; width:100%; }
    .error { border:1px solid var(--danger); color:var(--danger); padding:10px; }
    .empty { color:var(--muted); }
    article { border-top:1px solid var(--line); padding:20px 0; }
    article:first-of-type { border-top:0; padding-top:0; }
    article h3 { font-size:1.15rem; margin:0 0 6px; }
    .rule { color:var(--muted); font-size:.82rem; margin:0 0 14px; overflow-wrap:anywhere; }
    .rule b { color:var(--ink); }
    .feed-url { color:var(--acid); display:block; font-size:.82rem; overflow-wrap:anywhere; }
    .delete { background:none; color:var(--danger); margin-top:14px; padding:0; }
    @media (max-width:760px) { main { padding-top:36px; } .grid { grid-template-columns:1fr; } }
  </style>
</head>
<body>
<main>
  <header><h1>RSS<br><span>Studio</span></h1><p>Cut noise from podcast feeds. Match episode titles, publish clean RSS, subscribe from AntennaPod.</p></header>
  <div class="grid">
    <section>
      <h2>New output</h2>
      {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
      <form method="post" action="/feeds">
        <label for="name">Feed name</label><input id="name" name="name" maxlength="120" placeholder="My Foo Episodes" required>
        <label for="source_url">Source RSS URL</label><input id="source_url" name="source_url" type="url" maxlength="2048" placeholder="https://example.com/feed.xml" required>
        <label for="keyword">Title contains</label><input id="keyword" name="keyword" maxlength="200" placeholder="foo" required>
        <button type="submit">Create feed</button>
      </form>
    </section>
    <section>
      <h2>Published feeds / {{len .Feeds}}</h2>
      {{if not .Feeds}}<p class="empty">No outputs yet.</p>{{end}}
      {{range .Feeds}}
      <article>
        <h3>{{.Name}}</h3>
        <p class="rule">title contains <b>“{{.Keyword}}”</b><br>source: {{.SourceURL}}</p>
        <a class="feed-url" href="/feeds/{{.Slug}}.xml">{{$.BaseURL}}/feeds/{{.Slug}}.xml</a>
        <form method="post" action="/feeds/{{.Slug}}/delete"><button class="delete" type="submit">Delete</button></form>
      </article>
      {{end}}
    </section>
  </div>
</main>
</body>
</html>`))
