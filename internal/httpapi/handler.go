package httpapi

import (
	"errors"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/domain"
)

type Handler struct {
	service *application.Service
}

type homePageData struct {
	Feeds   []domain.Feed
	BaseURL string
	Error   string
}

func NewHandler(service *application.Service) http.Handler {
	handler := &Handler{service: service}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handler.home)
	mux.HandleFunc("POST /feeds", handler.createFeed)
	mux.HandleFunc("GET /feeds/{file}", handler.serveFeed)
	mux.HandleFunc("POST /feeds/{slug}/delete", handler.deleteFeed)
	return requestLogger(mux)
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
	if err := homeTemplate.Execute(writer, homePageData{Feeds: h.service.ListFeeds(), BaseURL: baseURL(request), Error: message}); err != nil {
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
  <header><h1>RSS<br><span>Studio</span></h1><p>Cut noise from podcast feeds. Match episode titles, publish clean RSS, subscribe from any RSS-compatible reader.</p></header>
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
