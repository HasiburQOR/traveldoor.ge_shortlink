// Minimal URL shortener: Go stdlib + SQLite (pure Go) + htmx admin.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

var (
	slugRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	reserved = map[string]bool{
		"admin": true, "api": true, "static": true, "healthz": true,
		"robots.txt": true, "favicon.ico": true,
	}
)

type Link struct {
	ID        int64
	Slug      string
	Target    string
	Title     string
	Clicks    int64
	CreatedAt string
	Short     string
}

type click struct {
	linkID  int64
	ref     string
	country string
	ua      string
}

type app struct {
	db      *sql.DB
	tpl     *template.Template
	baseURL string
	root    string
	user    string
	pass    string
	clicks  chan click
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	port := env("PORT", "8080")
	a := &app{
		baseURL: strings.TrimRight(env("BASE_URL", "http://localhost:"+port), "/"),
		root:    os.Getenv("ROOT_REDIRECT"),
		user:    env("ADMIN_USER", "admin"),
		pass:    os.Getenv("ADMIN_PASS"),
		clicks:  make(chan click, 1024),
		tpl:     template.Must(template.New("").Parse(templates)),
	}
	if a.pass == "" {
		log.Fatal("ADMIN_PASS is required")
	}

	dsn := "file:" + env("DB_PATH", "./links.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1) // SQLite: one writer, keeps things simple
	a.db = db
	if err := a.migrate(); err != nil {
		log.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		a.clickWorker()
		close(done)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /admin\n"))
	})
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /admin", a.auth(a.index))
	mux.HandleFunc("POST /admin/links", a.auth(a.create))
	mux.HandleFunc("DELETE /admin/links/{id}", a.auth(a.remove))
	mux.HandleFunc("GET /{slug}", a.redirect)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("listening on :%s (%s)", port, a.baseURL)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	close(a.clicks)
	<-done
	_ = db.Close()
}

func (a *app) migrate() error {
	_, err := a.db.Exec(`
CREATE TABLE IF NOT EXISTS links (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  slug       TEXT NOT NULL UNIQUE,
  target     TEXT NOT NULL,
  title      TEXT NOT NULL DEFAULT '',
  clicks     INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS clicks (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  link_id  INTEGER NOT NULL REFERENCES links(id) ON DELETE CASCADE,
  ts       TEXT NOT NULL DEFAULT (datetime('now')),
  referrer TEXT NOT NULL DEFAULT '',
  country  TEXT NOT NULL DEFAULT '',
  ua       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_clicks_link ON clicks(link_id);`)
	return err
}

// ---- public ----

func (a *app) home(w http.ResponseWriter, r *http.Request) {
	if a.root != "" {
		http.Redirect(w, r, a.root, http.StatusFound)
		return
	}
	w.Write([]byte("ok"))
}

func (a *app) redirect(w http.ResponseWriter, r *http.Request) {
	var id int64
	var target string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT id, target FROM links WHERE slug = ?`, r.PathValue("slug")).Scan(&id, &target)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Println("lookup:", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	select { // never block the redirect on logging
	case a.clicks <- click{id, r.Referer(), r.Header.Get("CF-IPCountry"), ua}:
	default:
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	http.Redirect(w, r, target, http.StatusFound)
}

func (a *app) clickWorker() {
	for c := range a.clicks {
		if _, err := a.db.Exec(`INSERT INTO clicks (link_id, referrer, country, ua) VALUES (?,?,?,?)`,
			c.linkID, c.ref, c.country, c.ua); err != nil {
			log.Println("click insert:", err)
			continue
		}
		if _, err := a.db.Exec(`UPDATE links SET clicks = clicks + 1 WHERE id = ?`, c.linkID); err != nil {
			log.Println("click count:", err)
		}
	}
}

// ---- admin ----

func (a *app) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(a.user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(a.pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// CSRF guard: state-changing requests must come from htmx (custom header)
		if r.Method != http.MethodGet && r.Header.Get("HX-Request") != "true" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT id, slug, target, title, clicks, created_at
		FROM links ORDER BY id DESC LIMIT 500`)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.ID, &l.Slug, &l.Target, &l.Title, &l.Clicks, &l.CreatedAt); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		l.Short = a.baseURL + "/" + l.Slug
		links = append(links, l)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	a.tpl.ExecuteTemplate(w, "page", map[string]any{"Links": links, "Host": strings.TrimPrefix(strings.TrimPrefix(a.baseURL, "https://"), "http://")})
}

func (a *app) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		a.fail(w, "The form could not be read. Reload the page and try again.")
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	slug := strings.TrimSpace(r.FormValue("slug"))
	title := strings.TrimSpace(r.FormValue("title"))

	if !validTarget(target) {
		a.fail(w, "Enter a full web address starting with http:// or https://.")
		return
	}
	if len(title) > 120 {
		a.fail(w, "Keep the label under 120 characters.")
		return
	}

	var l Link
	var err error
	if slug != "" {
		if !slugRe.MatchString(slug) || reserved[strings.ToLower(slug)] {
			a.fail(w, "Custom ending can use letters, numbers, - and _ only, and can't be a reserved word.")
			return
		}
		l, err = a.insert(slug, target, title)
		if isUnique(err) {
			a.fail(w, "That ending is already in use. Pick another.")
			return
		}
	} else {
		for i := 0; i < 5; i++ {
			l, err = a.insert(randSlug(6), target, title)
			if !isUnique(err) {
				break
			}
		}
	}
	if err != nil {
		log.Println("insert:", err)
		a.fail(w, "The link could not be saved. Try again.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	a.tpl.ExecuteTemplate(w, "row", l)
}

func (a *app) remove(w http.ResponseWriter, r *http.Request) {
	if _, err := a.db.Exec(`DELETE FROM links WHERE id = ?`, r.PathValue("id")); err != nil {
		log.Println("delete:", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK) // empty body: htmx removes the row
}

// fail sends the message into #msg (status 200 so htmx swaps it).
func (a *app) fail(w http.ResponseWriter, msg string) {
	w.Header().Set("HX-Retarget", "#msg")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	template.Must(template.New("e").Parse(`{{.}}`)).Execute(w, msg)
}

func (a *app) insert(slug, target, title string) (Link, error) {
	res, err := a.db.Exec(`INSERT INTO links (slug, target, title) VALUES (?,?,?)`, slug, target, title)
	if err != nil {
		return Link{}, err
	}
	id, _ := res.LastInsertId()
	return Link{
		ID: id, Slug: slug, Target: target, Title: title,
		CreatedAt: time.Now().UTC().Format("2006-01-02 15:04:05"),
		Short:     a.baseURL + "/" + slug,
	}, nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func validTarget(s string) bool {
	if s == "" || len(s) > 2048 {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func randSlug(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			log.Fatal(err)
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

// ---- templates ----

const templates = `
{{define "row"}}<tr id="link-{{.ID}}">
  <td class="short"><a href="{{.Short}}" target="_blank" rel="noopener">{{.Short}}</a>{{if .Title}}<span class="label">{{.Title}}</span>{{end}}</td>
  <td class="dest" title="{{.Target}}">{{.Target}}</td>
  <td class="num">{{.Clicks}}</td>
  <td class="when">{{slice .CreatedAt 0 10}}</td>
  <td class="act">
    <button type="button" class="ghost" onclick="copyLink(this,'{{.Short}}')">Copy</button>
    <button type="button" class="ghost danger" hx-delete="/admin/links/{{.ID}}" hx-confirm="Delete {{.Short}}? Anyone using it will get a 404." hx-target="#link-{{.ID}}" hx-swap="outerHTML">Delete</button>
  </td>
</tr>{{end}}

{{define "page"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Short links · {{.Host}}</title>
<script src="https://cdnjs.cloudflare.com/ajax/libs/htmx/2.0.4/htmx.min.js"></script>
<style>
:root{--bg:#f3f5f7;--panel:#fff;--ink:#14212b;--mute:#5d6b76;--line:#d9e0e5;--accent:#0b6e6e;--accent-ink:#fff;--danger:#a3321f}
@media (prefers-color-scheme:dark){:root{--bg:#0e161c;--panel:#16212a;--ink:#e6edf1;--mute:#93a3ad;--line:#26343e;--accent:#3ab5b0;--accent-ink:#06201f;--danger:#ff8a75}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{max-width:1040px;margin:0 auto;padding:32px 20px 64px}
h1{font-size:1.5rem;margin:0 0 4px;letter-spacing:-.01em}
.sub{color:var(--mute);margin:0 0 24px}
form{display:grid;grid-template-columns:2fr 1fr 1fr auto;gap:10px;background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px}
label{display:flex;flex-direction:column;gap:4px;font-size:.85rem;color:var(--mute)}
input{font:inherit;color:var(--ink);background:var(--bg);border:1px solid var(--line);border-radius:6px;padding:9px 10px;min-width:0}
input:focus-visible,button:focus-visible,a:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
button{font:inherit;cursor:pointer;border-radius:6px}
.primary{align-self:end;background:var(--accent);color:var(--accent-ink);border:0;padding:10px 18px;font-weight:600}
.ghost{background:none;border:1px solid var(--line);color:var(--ink);padding:5px 10px;font-size:.85rem}
.danger{color:var(--danger)}
#msg{min-height:1.4em;margin:10px 2px;color:var(--danger);font-size:.9rem}
.wrap{overflow-x:auto;background:var(--panel);border:1px solid var(--line);border-radius:10px}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:11px 14px;border-bottom:1px solid var(--line);vertical-align:top}
th{font-size:.82rem;font-weight:600;color:var(--mute)}
tr:last-child td{border-bottom:0}
a{color:var(--accent)}
.short{white-space:nowrap}
.label{display:block;color:var(--mute);font-size:.85rem}
.dest{max-width:340px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--mute)}
.num{font-variant-numeric:tabular-nums}
.act{white-space:nowrap;text-align:right}
.empty{padding:28px;text-align:center;color:var(--mute)}
@media (max-width:760px){form{grid-template-columns:1fr}.primary{width:100%}}
</style>
</head>
<body>
<main>
  <h1>Short links</h1>
  <p class="sub">Links on {{.Host}} redirect to the address you paste in.</p>
  <form hx-post="/admin/links" hx-target="#rows" hx-swap="afterbegin"
        hx-on::after-request="if(event.detail.xhr.getResponseHeader('HX-Retarget')===null){this.reset();document.getElementById('msg').textContent='';var e=document.getElementById('empty');if(e)e.remove()}">
    <label>Destination address<input name="target" type="url" required placeholder="https://traveldoor.ge/tours/…" autocomplete="off"></label>
    <label>Custom ending (optional)<input name="slug" placeholder="summer" autocomplete="off" maxlength="64"></label>
    <label>Label (optional)<input name="title" placeholder="Summer brochure" autocomplete="off" maxlength="120"></label>
    <button class="primary" type="submit">Create link</button>
  </form>
  <div id="msg" role="alert"></div>
  <div class="wrap">
    <table>
      <thead><tr><th>Short link</th><th>Destination</th><th>Clicks</th><th>Created</th><th></th></tr></thead>
      <tbody id="rows">{{range .Links}}{{template "row" .}}{{end}}</tbody>
    </table>
    {{if not .Links}}<div class="empty" id="empty">No links yet. Paste an address above to create the first one.</div>{{end}}
  </div>
</main>
<script>
function copyLink(btn,text){navigator.clipboard.writeText(text).then(function(){var t=btn.textContent;btn.textContent='Copied';setTimeout(function(){btn.textContent=t},1200)})}
</script>
</body>
</html>{{end}}
`
