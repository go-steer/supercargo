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

package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/googleauth"
	"github.com/go-steer/supercargo/internal/mcpserver"
	"github.com/go-steer/supercargo/internal/webui"
)

// googleClient is the company's Google OAuth client, if configured. A
// missing secret stops the node.
func googleClient(cfg *config.Config) (*googleauth.Client, error) {
	g := cfg.Google
	if g == nil {
		return nil, nil
	}
	secret, from, err := secretFrom(g.ClientSecretEnv, g.ClientSecretFile)
	if err != nil {
		return nil, fmt.Errorf("google: %w", err)
	}
	if secret == "" {
		return nil, fmt.Errorf("google: no client secret in %s", from)
	}
	return &googleauth.Client{ID: g.ClientID, Secret: secret}, nil
}

// secretFrom reads a secret from its environment variable, or from its
// file, which must be its owner's alone. from names where it came from.
func secretFrom(env, file string) (secret, from string, err error) {
	if env != "" {
		return os.Getenv(env), env, nil
	}
	fi, err := os.Stat(file)
	if err != nil {
		return "", "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", "", fmt.Errorf("%s is readable by others (mode %v); chmod 600 it", file, fi.Mode().Perm())
	}
	b, err := os.ReadFile(file) //nolint:gosec // G304: the operator's own configured file
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(b)), file, nil
}

// publicURL is the node's public origin, if Google sign-in names one.
func publicURL(cfg *config.Config) string {
	if cfg.Google == nil {
		return ""
	}
	return strings.TrimRight(cfg.Google.PublicURL, "/")
}

// publicHost is publicURL's host, for the UI's host check.
func publicHost(cfg *config.Config) []string {
	if u, err := url.Parse(publicURL(cfg)); err == nil && u.Hostname() != "" {
		return []string{strings.ToLower(u.Hostname())}
	}
	return nil
}

// webGoogle is the UI's Google sign-in, with web.auth: google.
func (a *App) webGoogle() *webui.Google {
	if a.google == nil || a.Config.Web == nil || a.Config.Web.Auth != config.AuthGoogle {
		return nil
	}
	return &webui.Google{Client: a.google, PublicURL: publicURL(a.Config)}
}

// mcpGoogle checks MCP callers' own Google tokens (mcp.server.auth:
// google): issued to the company's client, for a verified email that a
// persona acting as shipper has among its members. The tools are the
// shipper role's, so no other role's members may call them.
func (a *App) mcpGoogle() (func(context.Context, string) (string, time.Time, error), *mcpserver.Metadata) {
	if a.google == nil || a.Config.MCP.Server.Auth != config.AuthGoogle {
		return nil, nil
	}
	var members []string
	for _, p := range a.Config.Web.Personas {
		if slices.Contains(p.Roles, config.RoleShipper) {
			members = append(members, p.Members...)
		}
	}
	verify := func(ctx context.Context, token string) (string, time.Time, error) {
		id, err := a.google.AccessToken(ctx, token)
		if err != nil {
			return "", time.Time{}, err
		}
		if !googleauth.Member(id.Email, members) {
			return "", time.Time{}, errors.New("not a member of a persona acting as shipper")
		}
		return id.Email, time.Now().Add(googleauth.MaxTTL), nil
	}
	base := publicURL(a.Config)
	return verify, &mcpserver.Metadata{URL: base + mcpserver.MetadataPath + mcpserver.Path, Resource: base + mcpserver.Path,
		AuthorizationServers: []string{googleauth.Issuer}, Scopes: googleauth.Scopes}
}
