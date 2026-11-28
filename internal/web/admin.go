package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/KevinCFechtel/RSSProxy/internal/feed"
	"github.com/KevinCFechtel/RSSProxy/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Admin struct {
	Store        *store.Store
	Logger       *slog.Logger
	User         string
	Password     string
	SecureCookie bool
	templates    *template.Template
}

type field struct {
	Key, Label string
	Value      feed.Override
	Required   bool
}

type preview struct {
	GUID, Link, Key, Error string
}

type groupOption struct {
	ID       int64
	Name     string
	Path     string
	Selected bool
}

type page struct {
	Title                      string
	CSRF                       string
	Config                     feed.Config
	Feeds                      []store.Summary
	GroupOptions               []groupOption
	Fields                     []field
	Error                      string
	Preview                    []preview
	PreviewGUIDs, PreviewLinks string
}

func NewAdmin(db *store.Store, logger *slog.Logger, user, password string, secureCookie bool) (*Admin, error) {
	templates, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Admin{Store: db, Logger: logger, User: user, Password: password,
		SecureCookie: secureCookie, templates: templates}, nil
}

func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/{$}", a.list)
	mux.HandleFunc("GET /admin/feeds/new", a.form)
	mux.HandleFunc("GET /admin/groups/new", a.form)
	mux.HandleFunc("GET /admin/feeds/{id}", a.form)
	mux.HandleFunc("POST /admin/feeds/save", a.save)
	mux.HandleFunc("POST /admin/feeds/preview", a.preview)
	mux.HandleFunc("POST /admin/feeds/{id}/delete", a.delete)
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /admin/static/", http.StripPrefix("/admin/static/", http.FileServerFS(static)))
	return a.authenticate(http.NewCrossOriginProtection().Handler(a.csrf(mux)))
}

func (a *Admin) authenticate(next http.Handler) http.Handler {
	expectedUser, expectedPass := sha256.Sum256([]byte(a.User)), sha256.Sum256([]byte(a.Password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		user, pass, ok := r.BasicAuth()
		actualUser, actualPass := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
		userOK := subtle.ConstantTimeCompare(actualUser[:], expectedUser[:])
		passOK := subtle.ConstantTimeCompare(actualPass[:], expectedPass[:])
		if !ok || userOK&passOK != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="RSSProxy administration", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type csrfKey struct{}

func (a *Admin) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookieName := "rssproxy_csrf"
		if a.SecureCookie {
			cookieName = "__Host-rssproxy_csrf"
		}
		cookie, err := r.Cookie(cookieName)
		token := ""
		if err == nil && len(cookie.Value) == 64 {
			if _, err := hex.DecodeString(cookie.Value); err == nil {
				token = cookie.Value
			}
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "Invalid or oversized form", http.StatusBadRequest)
				return
			}
			actual := r.PostForm.Get("csrf")
			if token == "" || subtle.ConstantTimeCompare([]byte(actual), []byte(token)) != 1 {
				http.Error(w, "Invalid CSRF token", http.StatusForbidden)
				return
			}
		} else if token == "" {
			var bytes [32]byte
			_, _ = rand.Read(bytes[:])
			token = hex.EncodeToString(bytes[:])
			http.SetCookie(w, &http.Cookie{
				Name: cookieName, Value: token, Path: "/", HttpOnly: true,
				Secure: a.SecureCookie, SameSite: http.SameSiteStrictMode,
			})
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfKey{}, token)))
	})
}

func overrideFields(c feed.Config) []field {
	return []field{
		{"title", "Feed title", c.Overrides.Title, true},
		{"description", "Feed description", c.Overrides.Description, true},
		{"link", "Website link", c.Overrides.Link, true},
		{"image_url", "Feed image / Logo URL", c.Overrides.ImageURL, false},
		{"image_title", "Image title", c.Overrides.ImageTitle, false},
		{"image_link", "Image link", c.Overrides.ImageLink, false},
		{"item_image", "Article image URL", c.Overrides.ItemImage, false},
	}
}

func (a *Admin) render(w http.ResponseWriter, r *http.Request, name string, p page, status int) {
	p.CSRF, _ = r.Context().Value(csrfKey{}).(string)
	p.Fields = overrideFields(p.Config)
	if p.Config.Type == "group" && a.Store != nil {
		options, err := a.Store.List(r.Context())
		if err != nil {
			a.failure(w, err)
			return
		}
		// Keep configured priority on edit/preview, then offer unused sources.
		byID := make(map[int64]store.Summary, len(options))
		for _, option := range options {
			byID[option.Config.ID] = option
		}
		appendOption := func(option store.Summary) {
			if option.Config.Type == "" {
				p.GroupOptions = append(p.GroupOptions, groupOption{
					ID: option.Config.ID, Name: option.Config.Name, Path: option.Config.Path,
					Selected: slices.Contains(p.Config.SourceIDs, option.Config.ID),
				})
			}
		}
		for _, id := range p.Config.SourceIDs {
			if option, ok := byID[id]; ok {
				appendOption(option)
			}
		}
		for _, option := range options {
			if !slices.Contains(p.Config.SourceIDs, option.Config.ID) {
				appendOption(option)
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := a.templates.ExecuteTemplate(w, name, p); err != nil {
		a.Logger.Error("rendering administration failed")
	}
}

func (a *Admin) list(w http.ResponseWriter, r *http.Request) {
	feeds, err := a.Store.List(r.Context())
	if err != nil {
		a.failure(w, err)
		return
	}
	a.render(w, r, "list", page{Title: "Feeds", Feeds: feeds}, http.StatusOK)
}

func (a *Admin) form(w http.ResponseWriter, r *http.Request) {
	c := feed.DefaultConfig()
	if r.URL.Path == "/admin/groups/new" {
		c.Type = "group"
	}
	if value := r.PathValue("id"); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			http.NotFound(w, r)
			return
		}
		c, err = a.Store.ByID(r.Context(), id)
		if err != nil {
			a.failure(w, err)
			return
		}
	}
	title := "Configure feed"
	if c.Type == "group" {
		title = "Configure group"
	}
	a.render(w, r, "form", page{Title: title, Config: c}, http.StatusOK)
}

func configFromForm(r *http.Request) (feed.Config, error) {
	c := feed.DefaultConfig()
	var err error
	if value := r.PostForm.Get("id"); value != "" {
		c.ID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || c.ID < 0 {
			return c, errors.New("Invalid feed ID")
		}
	}
	if value := r.PostForm.Get("revision"); value != "" {
		c.Revision, err = strconv.ParseInt(value, 10, 64)
		if err != nil || c.Revision < 0 {
			return c, errors.New("Invalid configuration revision")
		}
	}
	c.Name = strings.TrimSpace(r.PostForm.Get("name"))
	c.Path = strings.TrimSpace(r.PostForm.Get("path"))
	c.URL = strings.TrimSpace(r.PostForm.Get("url"))
	c.Type = r.PostForm.Get("type")
	for _, value := range r.PostForm["source_ids"] {
		id, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || id <= 0 {
			return c, errors.New("Invalid group source ID")
		}
		c.SourceIDs = append(c.SourceIDs, id)
	}
	c.Identity.Source, c.Identity.Kind = r.PostForm.Get("identity_source"), r.PostForm.Get("identity_kind")
	c.Identity.Regex, c.Identity.Query = r.PostForm.Get("identity_regex"), strings.TrimSpace(r.PostForm.Get("identity_query"))
	c.Identity.Hosts = strings.FieldsFunc(strings.ToLower(r.PostForm.Get("identity_hosts")), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
	})
	c.Identity.LegacyDirection = r.PostForm.Get("legacy_direction")
	for _, value := range []struct {
		name string
		dest *int
	}{{"legacy_offset", &c.Identity.LegacyOffset}, {"legacy_length", &c.Identity.LegacyLength}} {
		if text := r.PostForm.Get(value.name); text != "" {
			*value.dest, err = strconv.Atoi(text)
			if err != nil {
				return c, errors.New("Invalid legacy offset or length")
			}
		}
	}
	fields := []struct {
		name string
		dest *feed.Override
	}{
		{"title", &c.Overrides.Title}, {"description", &c.Overrides.Description}, {"link", &c.Overrides.Link},
		{"image_url", &c.Overrides.ImageURL}, {"image_title", &c.Overrides.ImageTitle},
		{"image_link", &c.Overrides.ImageLink}, {"item_image", &c.Overrides.ItemImage},
	}
	for _, f := range fields {
		*f.dest = feed.Override{Mode: r.PostForm.Get(f.name + "_mode"), Value: r.PostForm.Get(f.name + "_value")}
	}
	c.Overrides.ItemImageSource = strings.TrimSpace(r.PostForm.Get("item_image_source"))
	return c, c.Validate()
}

func (a *Admin) save(w http.ResponseWriter, r *http.Request) {
	c, err := configFromForm(r)
	if err != nil {
		a.render(w, r, "form", page{Title: "Configure feed", Config: c, Error: err.Error()}, http.StatusBadRequest)
		return
	}
	_, err = a.Store.Save(r.Context(), c)
	if errors.Is(err, store.ErrInvalidGroup) {
		a.render(w, r, "form", page{Title: "Configure group", Config: c,
			Error: "Groups can contain only existing regular feeds."}, http.StatusBadRequest)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		a.render(w, r, "form", page{Title: "Configure feed", Config: c,
			Error: "Endpoint already in use or configuration has changed. Please reload."}, http.StatusConflict)
		return
	}
	if err != nil {
		a.failure(w, err)
		return
	}
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (a *Admin) preview(w http.ResponseWriter, r *http.Request) {
	c, err := configFromForm(r)
	p := page{Title: "Rule preview", Config: c,
		PreviewGUIDs: r.PostForm.Get("preview_guids"), PreviewLinks: r.PostForm.Get("preview_links")}
	if err != nil {
		p.Error = err.Error()
		a.render(w, r, "form", p, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(p.PreviewGUIDs+p.PreviewLinks) == "" && c.ID > 0 {
		l, err := a.Store.Lock(r.Context(), c.ID)
		if err != nil {
			a.failure(w, err)
			return
		}
		state, stateErr := l.State(r.Context())
		l.Close()
		if stateErr != nil {
			a.failure(w, stateErr)
			return
		}
		if state.Document != nil && state.Document.Feed != nil {
			for i, item := range state.Document.Feed.Items {
				if i >= 50 {
					break
				}
				p.Preview = append(p.Preview, preview{GUID: item.GUID, Link: item.Link})
			}
		}
	} else {
		guids, links := strings.Split(strings.ReplaceAll(p.PreviewGUIDs, "\r\n", "\n"), "\n"),
			strings.Split(strings.ReplaceAll(p.PreviewLinks, "\r\n", "\n"), "\n")
		for index := range min(50, max(len(guids), len(links))) {
			sample := preview{}
			if index < len(guids) {
				sample.GUID = strings.TrimSpace(guids[index])
			}
			if index < len(links) {
				sample.Link = strings.TrimSpace(links[index])
			}
			p.Preview = append(p.Preview, sample)
		}
	}
	for index := range p.Preview {
		sample := &p.Preview[index]
		sample.Key, err = c.Identity.Key(sample.GUID, sample.Link)
		if err != nil {
			sample.Error = err.Error()
		}
	}
	if len(p.Preview) == 0 {
		p.Error = "No source data available yet. Please enter example GUIDs or links."
	}
	a.render(w, r, "form", p, http.StatusOK)
}

func (a *Admin) delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	revision, revErr := strconv.ParseInt(r.PostForm.Get("revision"), 10, 64)
	if err != nil || revErr != nil || id <= 0 {
		http.Error(w, "Invalid feed ID or revision", http.StatusBadRequest)
		return
	}
	if err := a.Store.Delete(r.Context(), id, revision); err != nil {
		a.failure(w, err)
		return
	}
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (a *Admin) failure(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		http.Error(w, "Configuration changed or feed is used by a group; reload and remove group references before deleting", http.StatusConflict)
		return
	}
	a.Logger.Error("administration database operation failed")
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}
