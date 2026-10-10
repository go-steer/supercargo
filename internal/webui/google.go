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

package webui

import (
	"html/template"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/googleauth"
)

// Google is the UI's Google sign-in (web.auth: google).
type Google struct {
	Client *googleauth.Client
	// PublicURL is the node's origin as Google redirects back to it; the
	// redirect URI is PublicURL + CallbackPath.
	PublicURL string
}

// The sign-in's routes.
const (
	LoginPath    = Prefix + "auth/login"
	CallbackPath = Prefix + "auth/callback"
	// SignInTTL bounds how long a sign-in may take at Google.
	SignInTTL = 10 * time.Minute
)

func (s *server) redirectURI() string {
	return strings.TrimRight(s.cfg.Google.PublicURL, "/") + CallbackPath
}

// googleLogin sends the browser to Google, with what the callback will
// check in a cookie (only /ui/auth/ gets it over plain http; over https
// it is a __Host- cookie, for this host alone). Lax, not Strict: the callback is
// a navigation from Google's site, and a Strict cookie wouldn't come back.
func (s *server) googleLogin(w http.ResponseWriter, r *http.Request) {
	u, si := s.cfg.Google.Client.Start(s.redirectURI())
	//nolint:gosec // G124: Secure whenever the node is reached over https
	http.SetCookie(w, &http.Cookie{Name: s.signInCookie, Value: si.State + "." + si.Verifier + "." + si.Nonce, Path: s.signInPath,
		HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteLaxMode, MaxAge: int(SignInTTL / time.Second)})
	http.Redirect(w, r, u, http.StatusFound)
}

// googleCallback finishes the sign-in: the state must match the cookie's,
// the code is exchanged, the ID token verified, and the email must be a
// member of a persona. The page it answers then moves to the app with a
// same-site navigation, so the Strict session cookie goes along.
func (s *server) googleCallback(w http.ResponseWriter, r *http.Request) {
	//nolint:gosec // G124: as at login
	http.SetCookie(w, &http.Cookie{Name: s.signInCookie, Value: "", Path: s.signInPath, HttpOnly: true, Secure: s.cfg.Secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
	q := r.URL.Query()
	if q.Get("error") != "" {
		page(w, http.StatusUnauthorized, "Sign-in cancelled", "Google didn't sign you in.", false)
		return
	}
	var si googleauth.SignIn
	if c, err := r.Cookie(s.signInCookie); err == nil {
		if parts := strings.Split(c.Value, "."); len(parts) == 3 {
			si = googleauth.SignIn{State: parts[0], Verifier: parts[1], Nonce: parts[2]}
		}
	}
	id, err := s.cfg.Google.Client.Finish(r.Context(), s.redirectURI(), si, q.Get("state"), q.Get("code"))
	if err != nil {
		page(w, http.StatusUnauthorized, "Sign-in failed", "The sign-in didn't complete; try again.", false)
		return
	}
	i := slices.IndexFunc(s.cfg.Personas, func(p Persona) bool { return id.Member(p.Members) })
	if i < 0 {
		page(w, http.StatusForbidden, "Not allowed", id.Email+" may not sign in to "+s.cfg.Company+".", false)
		return
	}
	p := s.cfg.Personas[i]
	p.Email = id.Email
	s.startSession(w, r, p)
	page(w, http.StatusOK, "Signed in", "Signed in as "+id.Email+".", true)
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
{{if .Go}}<meta http-equiv="refresh" content="0;url=/ui/">{{end}}
<title>{{.Title}}</title><link rel="stylesheet" href="/ui/static/app.css"></head>
<body class="auth-page" data-theme="grayscale-dark"><main><h1>{{.Title}}</h1><p>{{.Text}}</p><p><a href="/ui/">{{if .Go}}Continue{{else}}Back to sign-in{{end}}</a></p></main></body></html>
`))

// page answers a small HTML page; with goOn, one that moves on to the app.
func page(w http.ResponseWriter, status int, title, text string, goOn bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, struct {
		Title, Text string
		Go          bool
	}{title, text, goOn})
}
