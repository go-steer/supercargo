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
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/assistant"
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
	// Hosts are the host names (no port) the UI may be reached as besides
	// localhost, 127.0.0.1, ::1 and any *.localhost name: the node's
	// listen address, or a proxy's name for it (a Cloud Workstations port,
	// say). A request's Host, and its Origin if it has one, must be one of
	// them, so a web page that rebinds its own name to this machine can't
	// reach the UI.
	Hosts []string
	// FrameAncestors are origins that may frame this UI (a presenter page
	// on another node); without them only this UI's own pages may.
	FrameAncestors []string
	// Panels, if any, are the UIs /ui/presenter shows side by side.
	Panels []Panel
	// NewAssistant makes the shipper role's assistant for one session (nil:
	// the UI has no assistant). Each session gets its own, with its own
	// conversation and drafts.
	NewAssistant func() (*assistant.Assistant, error)
	Now          func() time.Time
}

// Panel is one UI on the presenter page.
type Panel struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Session limits: a session lasts SessionTTL; at most MaxSessions are
// kept (sign-in needs no password in demo mode, so its cost is bounded),
// each with at most MaxStreams live activity streams. A stream is
// re-checked every StreamCheck and ends with its session.
const (
	SessionTTL  = 12 * time.Hour
	MaxSessions = 1000
	MaxStreams  = 4
)

// StreamCheck is how often a live stream re-checks its session.
var StreamCheck = 5 * time.Second

type session struct {
	persona Persona
	expires time.Time
	streams int
	chat    *chat // the session's assistant, made on first use
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
	mux.HandleFunc("GET /ui/presenter", s.presenter)
	mux.HandleFunc("GET /ui/api/presenter", s.presenterPanels)
	mux.HandleFunc("GET /ui/static/{$}", http.NotFound) // no listing, no stray index
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
	mux.HandleFunc("GET /ui/api/bookings", s.authed(s.forward("/bookings", []string{RoleOperator})))
	mux.HandleFunc("GET /ui/api/bookings/{ref}/events", s.authed(s.forward("/bookings/{ref}/events", []string{RoleOperator})))
	mux.HandleFunc("GET /ui/api/shipments/{operator}/{ref}/journey", s.authed(s.forward("/shipments/{operator}/{ref}/journey", shipper)))
	mux.HandleFunc("GET /ui/api/bookings/{ref}/journey", s.authed(s.forward("/bookings/{ref}/journey", []string{RoleOperator})))
	// The assistant is the shipper role's (a receiver's comes later, #75).
	asShipper := []string{RoleShipper}
	mux.HandleFunc("GET /ui/api/assistant", s.authed(s.role(asShipper, s.assistantState)))
	mux.HandleFunc("POST /ui/api/assistant/turn", s.authed(s.role(asShipper, s.assistantTurn)))
	mux.HandleFunc("POST /ui/api/assistant/send", s.authed(s.role(asShipper, s.assistantSend)))
	mux.HandleFunc("POST /ui/api/assistant/discard", s.authed(s.role(asShipper, s.assistantDiscard)))
	mux.HandleFunc("POST /ui/api/disrupt", s.authed(s.role([]string{RoleOperator}, s.disrupt)))
	return s.hostCheck(s.headers(mux))
}

// hostCheck refuses a request naming a host the UI isn't served as. A page
// on another site can rebind its own name to this machine and then pass
// any Origin check (its origin is "this host" to the browser); it can't
// make the browser send a Host it doesn't own.
func (s *server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedHost reports whether hostport names a host the UI is served as.
func (s *server) allowedHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost") ||
		slices.Contains(s.cfg.Hosts, host)
}

// headers sets what every UI response carries: no inline script or style,
// no framing by other origins, no sniffing, no referrer.
func (s *server) headers(next http.Handler) http.Handler {
	ancestors := strings.Join(append([]string{"'self'"}, s.cfg.FrameAncestors...), " ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; "+
			"frame-ancestors "+ancestors+"; base-uri 'none'; form-action 'self'")
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

// presenter serves the presenter page: the configured UIs, each in a
// frame (and only those: its CSP allows framing nothing else).
func (s *server) presenter(w http.ResponseWriter, _ *http.Request) {
	if len(s.cfg.Panels) == 0 {
		http.NotFound(w, nil)
		return
	}
	b, err := static.ReadFile("static/presenter.html")
	if err != nil {
		http.Error(w, "no presenter page", http.StatusInternalServerError)
		return
	}
	var origins []string
	for _, p := range s.cfg.Panels {
		if u, err := url.Parse(p.URL); err == nil {
			origins = append(origins, u.Scheme+"://"+u.Host)
		}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; "+
		"frame-src "+strings.Join(origins, " ")+"; frame-ancestors 'self'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// PresenterResponse is the presenter page's panels.
type PresenterResponse struct {
	Panels []Panel `json:"panels"`
}

func (s *server) presenterPanels(w http.ResponseWriter, _ *http.Request) {
	control.Answer(w, http.StatusOK, PresenterResponse{Panels: append([]Panel{}, s.cfg.Panels...)})
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
// sent: it must be JSON, and an Origin header, if any, must be this
// request's host and port exactly (another port on localhost is another
// site, such as the other node's UI), or one of web.hosts with no port: a
// proxy forwards the page's https://<proxy name> as http://localhost:port.
// (The session cookie is SameSite=Strict too.)
func (s *server) sameOrigin(r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) || (u.Port() == "" && slices.Contains(s.cfg.Hosts, strings.ToLower(u.Hostname())))
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
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
	// Signing in again replaces the session the browser had.
	if c, err := r.Cookie(s.cookie); err == nil {
		s.drop(c.Value)
	}
	s.sessions[id] = &session{persona: s.cfg.Personas[i], expires: s.cfg.Now().Add(SessionTTL)}
	s.order = append(s.order, id)
	if len(s.order) > MaxSessions {
		// Expired sessions go first; past that, the oldest.
		now := s.cfg.Now()
		s.order = slices.DeleteFunc(s.order, func(o string) bool {
			if sess, ok := s.sessions[o]; !ok || !now.Before(sess.expires) {
				delete(s.sessions, o)
				return true
			}
			return false
		})
		for len(s.order) > MaxSessions {
			s.drop(s.order[0])
		}
	}
	s.mu.Unlock()
	//nolint:gosec // G124: Secure whenever the node serves TLS; insecure-dev is plain HTTP, local demos only
	http.SetCookie(w, &http.Cookie{Name: s.cookie, Value: id, Path: Prefix, HttpOnly: true, Secure: s.cfg.Secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(SessionTTL / time.Second)})
	control.Answer(w, http.StatusOK, s.cfg.Personas[i])
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		control.Answer(w, http.StatusForbidden, errorBody("cross-site request refused"))
		return
	}
	if c, err := r.Cookie(s.cookie); err == nil {
		s.mu.Lock()
		s.drop(c.Value)
		s.mu.Unlock()
	}
	//nolint:gosec // G124: as at sign-in
	http.SetCookie(w, &http.Cookie{Name: s.cookie, Value: "", Path: Prefix, HttpOnly: true, Secure: s.cfg.Secure,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
	control.Answer(w, http.StatusOK, struct{}{})
}

// drop ends session id. Call with mu held.
func (s *server) drop(id string) {
	delete(s.sessions, id)
	if i := slices.Index(s.order, id); i >= 0 {
		s.order = slices.Delete(s.order, i, i+1)
	}
}

// sessionID is the request's session cookie value.
func (s *server) sessionID(r *http.Request) string {
	if c, err := r.Cookie(s.cookie); err == nil {
		return c.Value
	}
	return ""
}

// lookup is session id's persona, if the session is live. Call with mu held.
func (s *server) lookup(id string) (*session, bool) {
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	if !s.cfg.Now().Before(sess.expires) {
		s.drop(id)
		return nil, false
	}
	return sess, true
}

// current is the request's signed-in persona.
func (s *server) current(r *http.Request) (Persona, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.lookup(s.sessionID(r))
	if !ok {
		return Persona{}, false
	}
	return sess.persona, true
}

// stream runs a live stream for session id: at most MaxStreams
// at once, and cancelled once the session ends (sign-out, expiry, being
// pushed out), checked every StreamCheck.
func (s *server) stream(w http.ResponseWriter, id string, r *http.Request, serve func(*http.Request)) {
	s.mu.Lock()
	sess, ok := s.lookup(id)
	if ok && sess.streams >= MaxStreams {
		ok = false
	}
	if ok {
		sess.streams++
	}
	s.mu.Unlock()
	if !ok {
		control.Answer(w, http.StatusTooManyRequests, errorBody("too many live streams for this session"))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer func() {
		cancel()
		s.mu.Lock()
		sess.streams--
		s.mu.Unlock()
	}()
	go func() {
		t := time.NewTicker(StreamCheck)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.mu.Lock()
				_, live := s.lookup(id)
				s.mu.Unlock()
				if !live {
					cancel()
					return
				}
			}
		}
	}()
	serve(r.WithContext(ctx))
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

// role lets h run only for a persona with one of roles.
func (s *server) role(roles []string, h func(http.ResponseWriter, *http.Request, Persona)) func(http.ResponseWriter, *http.Request, Persona) {
	return func(w http.ResponseWriter, r *http.Request, p Persona) {
		if !slices.ContainsFunc(p.Roles, func(role string) bool { return slices.Contains(roles, role) }) {
			control.Answer(w, http.StatusForbidden, errorBody("your roles don't include this"))
			return
		}
		h(w, r, p)
	}
}

// MeResponse is the signed-in persona and what it sees.
type MeResponse struct {
	Company   string   `json:"company"`
	Domain    string   `json:"domain"`
	Persona   Persona  `json:"persona"`
	Sides     []string `json:"sides"`
	Demo      bool     `json:"demo"`
	Assistant bool     `json:"assistant"` // the persona has the shipper's assistant
}

func (s *server) me(w http.ResponseWriter, _ *http.Request, p Persona) {
	control.Answer(w, http.StatusOK, MeResponse{Company: s.cfg.Company, Domain: s.cfg.Domain, Persona: p, Sides: p.Sides(), Demo: true,
		Assistant: s.cfg.NewAssistant != nil && slices.Contains(p.Roles, RoleShipper)})
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
		// No sides would mean no filter, and so every side: refuse.
		if len(p.Sides()) == 0 {
			control.Answer(w, http.StatusForbidden, errorBody("your persona acts in no role"))
			return
		}
		// The control API matches on the escaped path: fill both forms, so
		// a value with a "/" stays one segment.
		target, escaped := path, path
		for _, name := range []string{"seq", "operator", "ref"} {
			v := r.PathValue(name)
			if v == "." || v == ".." {
				control.Answer(w, http.StatusBadRequest, errorBody("not a valid "+name))
				return
			}
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
		if path == "/events" {
			s.stream(w, s.sessionID(r), r2, func(r *http.Request) { s.cfg.Control.ServeHTTP(w, r) })
			return
		}
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
