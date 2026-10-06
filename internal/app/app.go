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

// Package app assembles a running node from its configuration: keys,
// discovery, the REST client, the role's handlers, and the HTTP server.
package app

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// App is a configured node and what it needs to run.
type App struct {
	Config *config.Config
	Node   *node.Node
	// Shipper is set for the shipper role.
	Shipper *shipper.Shipper
	// Bookings are the node's own (the operator's confirmed, or the
	// shipper's recorded).
	Bookings *store.Bookings
	// Keys is the key cache (partners pinned).
	Keys *discovery.Cache
	// Events are the booking events and alerts the node received (shipper)
	// or published (operator).
	Events *store.EventLog
	Server *http.Server
	Logger *slog.Logger
}

// New builds an App from cfg.
func New(cfg *config.Config, logger *slog.Logger) (*App, error) {
	priv, err := config.LoadSigningKey(cfg.Keys[0].PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	signer, err := sig.NewEd25519Signer(priv)
	if err != nil {
		return nil, err
	}
	kid := cfg.Keys[0].KID
	jwk, err := sig.Ed25519JWK(priv.Public().(ed25519.PublicKey), kid)
	if err != nil {
		return nil, err
	}

	insecure := cfg.TLS.Mode == config.TLSInsecureDev
	var clientTLS *tls.Config
	if cfg.TLS.CAFile != "" {
		pool, err := loadPool(cfg.TLS.CAFile)
		if err != nil {
			return nil, err
		}
		clientTLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	// The fetcher and the REST client get the same peers and TLS policy:
	// a profile fetched over one policy must not be posted to over another.
	fetcher, err := discovery.NewFetcher(discovery.FetcherOptions{Peers: cfg.Peers, AllowHTTP: insecure, TLSConfig: clientTLS})
	if err != nil {
		return nil, err
	}
	cache, err := discovery.NewCache(fetcher, discovery.CacheOptions{
		TTL: cfg.Discovery.TTL, RefreshFloor: cfg.Discovery.RefreshFloor, Pinned: cfg.Pinned(),
	})
	if err != nil {
		return nil, err
	}
	client, err := rest.NewClient(cache, rest.ClientOptions{Peers: cfg.Peers, AllowHTTP: insecure, TLSConfig: clientTLS})
	if err != nil {
		return nil, err
	}

	a := &App{Config: cfg, Bookings: store.NewBookings(), Events: store.NewEventLog(0), Keys: cache, Logger: logger}
	handlers := map[string]node.Handler{}
	if cfg.Role == config.RoleOperator {
		o := cfg.Operator
		deny := map[string]bool{}
		for _, d := range o.Deny {
			deny[d] = true
		}
		handlers[intents.BookingRequest] = &operator.BookingService{
			Bookings: a.Bookings, Deny: deny, RefPrefix: o.RefPrefix, ContainerPrefix: o.ContainerPrefix, Now: time.Now,
			Voyage: operator.Voyage{Vessel: o.Vessel, Number: o.Voyage, DepartureHour: o.DepartureHour, Transit: o.Transit},
		}
	}
	if cfg.Role == config.RoleShipper {
		inbound := &shipper.Inbound{Bookings: a.Bookings, Log: a.Events, Now: time.Now}
		handlers[intents.TnTEvent] = inbound
		handlers[intents.TnTAlert] = inbound
	}
	restURL := ""
	if insecure {
		restURL = "http://" + cfg.Domain + "/ulp"
	}
	n, err := node.New(node.Config{
		Domain: cfg.Domain, Name: cfg.Name, Roles: []string{cfg.Role},
		KID: kid, Signer: signer, PublicJWK: jwk, RESTURL: restURL, AllowHTTP: insecure,
		Keys:     cache,
		Replay:   store.NewReplay(store.ReplayOptions{Retention: cfg.Security.ReplayRetention}),
		MaxSkew:  cfg.Security.MaxSkew,
		Handlers: handlers,
		Poster:   client,
		Observer: node.SlogObserver(logger),
	})
	if err != nil {
		return nil, err
	}
	a.Node = n
	if cfg.Role == config.RoleShipper {
		a.Shipper = &shipper.Shipper{Node: n, Bookings: a.Bookings, Now: time.Now}
	}
	a.Server = &http.Server{
		Addr:    cfg.Listen,
		Handler: n.Handler(),
		// Every timeout set: a client that trickles bytes must not hold a
		// connection (and its goroutine) open indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		// Info, not Warn: a client failing its TLS handshake is not the
		// node's problem.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelInfo),
	}
	return a, nil
}

// Serve runs the HTTP server until ctx is done, then shuts it down
// gracefully.
func (a *App) Serve(ctx context.Context) error {
	cfg := a.Config
	a.Logger.Info("node starting", "domain", cfg.Domain, "role", cfg.Role, "listen", cfg.Listen, "tls", cfg.TLS.Mode)
	if cfg.TLS.Mode == config.TLSInsecureDev {
		a.Logger.Warn("INSECURE DEVELOPMENT MODE: no TLS. Nothing ties a profile, or its keys, to its domain. Never use this outside a local demo.")
	}
	if len(cfg.Peers) > 0 {
		// The peer map bypasses the outbound address guard for these
		// domains; it belongs in local deployments only, and says so.
		for _, d := range slices.Sorted(maps.Keys(cfg.Peers)) {
			a.Logger.Warn("peer map entry: connecting to this address instead of resolving the domain", "domain", d, "target", cfg.Peers[d])
		}
	}
	errc := make(chan error, 1)
	go func() {
		var err error
		if cfg.TLS.Mode == config.TLSLocalCA {
			err = a.Server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			err = a.Server.ListenAndServe()
		}
		errc <- err
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := a.Server.Shutdown(shutdown) //nolint:contextcheck // shutdown must outlive the cancelled ctx
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		a.Logger.Info("node stopped")
		return err
	}
}

func loadPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator's configured CA file
	if err != nil {
		return nil, fmt.Errorf("app: CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("app: CA file %s holds no PEM certificates", path)
	}
	return pool, nil
}
