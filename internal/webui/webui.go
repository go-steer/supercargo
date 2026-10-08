// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package webui is the browser UI a node serves its own people under
// /ui/ (docs/DESIGN.md, A.12; #30): the static app, sign-in, and an API
// that forwards to the node's control API once it has checked who is
// asking and what they may see.
//
// A person signs in as a persona, which acts in some of the node's roles.
// Every read is cut to those roles' sides before the control API sees it,
// whatever the browser asked for: the node, not the page, decides what a
// persona sees. Sending stays in Go, behind the control API's own rules.
package webui

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/control"
)

//go:embed static
var static embed.FS

// Prefix is where the UI is served.
const Prefix = "/ui/"

// Roles, as the node's config names them.
const (
	RoleOperator = "operator"
	RoleShipper  = "shipper"
	RoleReceiver = "receiver"
)

// Persona is someone who may sign in.
type Persona struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Title string   `json:"title,omitempty"`
	Roles []string `json:"roles"`
}

// Sides are the activity and message sides the persona's roles see.
func (p Persona) Sides() []string {
	var s []string
	for _, r := range p.Roles {
		side := activity.SideCustomer
		if r == RoleOperator {
			side = activity.SideOperator
		}
		if !slices.Contains(s, side) {
			s = append(s, side)
		}
	}
	return s
}

// Config is what the UI needs from its node.
type Config struct {
	Company, Domain string
	Personas        []Persona
	// Control is the node's control API (trusted): the UI reaches it only
	// after authorizing each request.
	Control http.Handler
	// Secure marks the session cookie Secure (the node serves TLS).
	Secure bool
	Now    func() time.Time
}

// Session limits: a session lasts SessionTTL; at most MaxSessions are
// kept (sign-in needs no password in demo mode, so its cost is bounded).
const (
	SessionTTL  = 12 * time.Hour
	MaxSessions = 1000
)

type session struct {
	persona Persona
	expires time.Time
}

type server struct {
	cfg    Config
	cookie string
	files  http.Handler

	mu       sync.Mutex
	sessions map[string]*session
	order    []string // session IDs, oldest first
}

// Handler serves the UI under Prefix.
func Handler(cfg Config) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	sub, _ := fs.Sub(static, "static") // the embedded directory exists
	s := &server{cfg: cfg, sessions: map[string]*session{}, files: http.StripPrefix(Prefix+"static/", http.FileServerFS(sub)),
		// Per node: browsers share cookies across ports on one host, and
		// the local demo runs every node on 127.0.0.1.
		cookie: "ulp_session_" + strings.NewReplacer(".", "_", "-", "_").Replace(cfg.Domain)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui/{$}", s.index)
	mux.Handle("GET /ui/static/", s.files)
	mux.HandleFunc("GET /ui/api/personas", s.personas)
	mux.HandleFunc("POST /ui/api/login", s.login)
	mux.HandleFunc("POST /ui/api/logout", s.logout)
	mux.HandleFunc("GET /ui/api/me", s.authed(s.me))
	mux.HandleFunc("GET /ui/api/messages", s.authed(s.forward("/messages", nil)))
	mux.HandleFunc("GET /ui/api/messages/{seq}", s.authed(s.forward("/messages/{seq}", nil)))
	mux.HandleFunc("GET /ui/api/events", s.authed(s.forward("/events", nil)))
	shipper := []string{RoleShipper, RoleReceiver}
	mux.HandleFunc("GET /ui/api/shipments", s.authed(s.forward("/shipments", shipper)))
	mux.HandleFunc("GET /ui/api/shipments/{operator}/{ref}/events", s.authed(s.forward("/shipments/{operator}/{ref}/events", shipper)))
	mux.HandleFunc("GET /ui/api/profile/{domain}", s.authed(s.forward("/profile/{domain}", shipper)))
	mux.HandleFunc("GET /ui/api/bookings", s.authed(s.forward("/bookings", []string{RoleOperator})))
	mux.HandleFunc("GET /ui/api/bookings/{ref}/events", s.authed(s.forward("/bookings/{ref}/events", []string{RoleOperator})))
	return headers(mux)
}

// headers sets what every UI response carries: no inline script or style,
// no framing by other origins, no sniffing, no referrer.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; "+
			"frame-ancestors 'self'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/ui/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// index serves the app's page (FileServer would redirect index.html).
func (s *server) index(w http.ResponseWriter, _ *http.Request) {
	b, err := static.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "no UI", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// PersonasResponse is who may sign in, and to which company.
type PersonasResponse struct {
	Company  string    `json:"company"`
	Domain   string    `json:"domain"`
	Personas []Persona `json:"personas"`
	Demo     bool      `json:"demo"`
}

func (s *server) personas(w http.ResponseWriter, _ *http.Request) {
	control.Answer(w, http.StatusOK, PersonasResponse{Company: s.cfg.Company, Domain: s.cfg.Domain, Personas: s.cfg.Personas, Demo: true})
}

// sameOrigin rejects a state-changing request another site could have
// sent: it must be JSON, and an Origin header, if any, must be this host.
// (The session cookie is SameSite=Strict too.)
func sameOrigin(r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		control.Answer(w, http.StatusForbidden, errorBody("cross-site request refused"))
		return
	}
	var req struct {
		Persona string `json:"persona"`
	}
	if err := control.Decode(r, &req); err != nil {
		control.Answer(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	i := slices.IndexFunc(s.cfg.Personas, func(p Persona) bool { return p.ID == req.Persona })
	if i < 0 {
		control.Answer(w, http.StatusForbidden, errorBody("no such persona"))
		return
	}
	id := newID()
	s.mu.Lock()
	s.sessions[id] = &session{persona: s.cfg.Personas[i], expires: s.cfg.Now().Add(SessionTTL)}
	s.order = append(s.order, id)
	for len(s.order) > MaxSessions {
		delete(s.sessions, s.order[0])
		s.order = s.order[1:]
	}
	s.mu.Unlock()
	//nolint:gosec // G124: Secure whenever the node serves TLS; insecure-dev is plain HTTP, local demos only
	http.SetCookie(w, &http.Cookie{Name: s.cookie, Value: id, Path: Prefix, HttpOnly: true, Secure: s.cfg.Secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(SessionTTL / time.Second)})
	control.Answer(w, http.StatusOK, s.cfg.Personas[i])
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		control.Answer(w, http.StatusForbidden, errorBody("cross-site request refused"))
		return
	}
	if c, err := r.Cookie(s.cookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	//nolint:gosec // G124: as at sign-in
	http.SetCookie(w, &http.Cookie{Name: s.cookie, Value: "", Path: Prefix, HttpOnly: true, Secure: s.cfg.Secure,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
	control.Answer(w, http.StatusOK, struct{}{})
}

// current is the request's signed-in persona.
func (s *server) current(r *http.Request) (Persona, bool) {
	c, err := r.Cookie(s.cookie)
	if err != nil {
		return Persona{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[c.Value]
	if !ok {
		return Persona{}, false
	}
	if !s.cfg.Now().Before(sess.expires) {
		delete(s.sessions, c.Value)
		return Persona{}, false
	}
	return sess.persona, true
}

func (s *server) authed(h func(http.ResponseWriter, *http.Request, Persona)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.current(r)
		if !ok {
			control.Answer(w, http.StatusUnauthorized, errorBody("sign in first"))
			return
		}
		h(w, r, p)
	}
}

// MeResponse is the signed-in persona and what it sees.
type MeResponse struct {
	Company string   `json:"company"`
	Domain  string   `json:"domain"`
	Persona Persona  `json:"persona"`
	Sides   []string `json:"sides"`
	Demo    bool     `json:"demo"`
}

func (s *server) me(w http.ResponseWriter, _ *http.Request, p Persona) {
	control.Answer(w, http.StatusOK, MeResponse{Company: s.cfg.Company, Domain: s.cfg.Domain, Persona: p, Sides: p.Sides(), Demo: true})
}

// forward passes the request to the control API's path (its {wildcards}
// filled from this request's), if the persona has one of roles (nil: any
// role). Its side parameters are replaced with the persona's sides.
func (s *server) forward(path string, roles []string) func(http.ResponseWriter, *http.Request, Persona) {
	return func(w http.ResponseWriter, r *http.Request, p Persona) {
		if roles != nil && !slices.ContainsFunc(p.Roles, func(role string) bool { return slices.Contains(roles, role) }) {
			control.Answer(w, http.StatusForbidden, errorBody("your roles don't include this"))
			return
		}
		// The control API matches on the escaped path: fill both forms, so
		// a value with a "/" stays one segment.
		target, escaped := path, path
		for _, name := range []string{"seq", "operator", "ref", "domain"} {
			v := r.PathValue(name)
			target = strings.ReplaceAll(target, "{"+name+"}", v)
			escaped = strings.ReplaceAll(escaped, "{"+name+"}", url.PathEscape(v))
		}
		q := r.URL.Query()
		q.Del("side")
		for _, side := range p.Sides() {
			q.Add("side", side)
		}
		r2 := r.Clone(r.Context())
		r2.URL = &url.URL{Path: target, RawPath: escaped, RawQuery: q.Encode()}
		r2.RequestURI = ""
		r2.Header.Del("Cookie")
		s.cfg.Control.ServeHTTP(w, r2)
	}
}

func newID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return base64.RawURLEncoding.EncodeToString(b)
}

type errorJSON struct {
	Error string `json:"error"`
}

func errorBody(msg string) errorJSON { return errorJSON{Error: msg} }
