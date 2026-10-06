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
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
	"github.com/go-steer/supercargo/pkg/transport/a2a"
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

	// The operator's push path and simulator, and the presenter's control
	// API (nil unless configured).
	Publisher *operator.Publisher
	Simulator *operator.Simulator
	Control   *http.Server
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
	// Exchanges get 10s unless their context says otherwise; an inquiry,
	// waiting on the operator's model, sets a longer deadline (shipper.AskTimeout).
	clientOpts := rest.ClientOptions{Peers: cfg.Peers, AllowHTTP: insecure, TLSConfig: clientTLS, MaxTimeout: time.Minute}
	client, err := rest.NewClient(cache, clientOpts)
	if err != nil {
		return nil, err
	}
	a2aClient, err := a2a.NewClient(cache, clientOpts)
	if err != nil {
		return nil, err
	}

	a := &App{Config: cfg, Bookings: store.NewBookings(), Events: store.NewEventLog(0), Keys: cache, Logger: logger}
	handlers := map[string]node.Handler{}
	if cfg.Role == config.RoleOperator {
		a.operatorRole(handlers)
	}
	if cfg.Role == config.RoleShipper {
		inbound := &shipper.Inbound{Bookings: a.Bookings, Log: a.Events, Now: time.Now,
			OnEvent: func(b store.Booking, e *intents.TnTEventBody) { logEvent(logger, "event recorded", b.Ref, e) },
			OnAlert: func(b store.Booking, al *intents.TnTAlertBody) { logAlert(logger, "alert recorded", b.Ref, al) },
		}
		handlers[intents.TnTEvent] = inbound
		handlers[intents.TnTAlert] = inbound
	}
	restURL, a2aURL := "", ""
	if cfg.Role == config.RoleOperator {
		// The operator's agent serves the shipment-inquiry skill.
		a2aURL = "https://" + cfg.Domain + "/a2a"
	}
	if insecure {
		restURL = "http://" + cfg.Domain + "/ulp"
		if a2aURL != "" {
			a2aURL = "http://" + cfg.Domain + "/a2a"
		}
	}
	n, err := node.New(node.Config{
		Domain: cfg.Domain, Name: cfg.Name, Roles: []string{cfg.Role},
		KID: kid, Signer: signer, PublicJWK: jwk, RESTURL: restURL, A2AURL: a2aURL, AllowHTTP: insecure,
		Keys:     cache,
		Replay:   store.NewReplay(store.ReplayOptions{Retention: cfg.Security.ReplayRetention}),
		MaxSkew:  cfg.Security.MaxSkew,
		Handlers: handlers,
		Poster:   client,
		A2A:      a2aClient,
		Observer: node.SlogObserver(logger),
	})
	if err != nil {
		return nil, err
	}
	a.Node = n
	if a.Publisher != nil {
		a.Publisher.Pusher = n
	}
	if cfg.Role == config.RoleShipper {
		a.Shipper = &shipper.Shipper{Node: n, Bookings: a.Bookings, Events: a.Events, Now: time.Now}
	}
	if cfg.Control != nil {
		h := operator.ControlHandler(a.Simulator)
		if a.Shipper != nil {
			h = shipper.ControlHandler(a.Shipper)
		}
		a.Control = control.Server(h, slog.NewLogLogger(logger.Handler(), slog.LevelInfo))
	}
	a.Server = &http.Server{
		Addr:    cfg.Listen,
		Handler: n.Handler(),
		// Every timeout set: a client that trickles bytes must not hold a
		// connection (and its goroutine) open indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second, // an inquiry waits on a model
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
	errc := make(chan error, 2)
	if a.Control != nil {
		// Opened first: a socket that can't be opened stops the node before
		// it serves anything.
		l, err := control.Listen(cfg.Control.Socket)
		if err != nil {
			return err
		}
		a.Logger.Info("control API listening (local only: whoever can reach the socket controls this node)", "socket", cfg.Control.Socket)
		go func() { errc <- fmt.Errorf("control API: %w", a.Control.Serve(l)) }()
	}
	go func() {
		var err error
		if cfg.TLS.Mode == config.TLSLocalCA {
			err = a.Server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			err = a.Server.ListenAndServe()
		}
		errc <- err
	}()
	if a.Simulator != nil {
		go a.Simulator.Run()
	}
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	a.shutdown() //nolint:contextcheck // shutdown must outlive the cancelled ctx
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	a.Logger.Info("node stopped")
	return err
}

// shutdown stops the simulator (no new events), then the publisher (what
// is still queued is reported as dropped), then the servers.
func (a *App) shutdown() {
	if a.Simulator != nil {
		a.Simulator.Close()
	}
	if a.Publisher != nil {
		a.Publisher.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if a.Control != nil {
		_ = a.Control.Shutdown(ctx)
	}
	_ = a.Server.Shutdown(ctx)
}

// operatorRole sets up the booking service, the publisher and simulator
// behind it. The publisher's Pusher is the node, set once it exists.
func (a *App) operatorRole(handlers map[string]node.Handler) {
	cfg, log := a.Config, a.Logger
	o := cfg.Operator
	a.Publisher = &operator.Publisher{Domain: cfg.Domain, Log: a.Events, Now: time.Now,
		OnDrop: func(ref, id string, err error) {
			log.Warn("push dropped", "booking", ref, "id", id, "err", err)
		},
	}
	a.Simulator = &operator.Simulator{Publisher: a.Publisher, HoursPerSecond: cfg.Simulator.HoursPerSecond,
		OnError: func(ref string, err error) { log.Warn("event not published", "booking", ref, "err", err) },
		OnPublish: func(ref string, body intents.Body) {
			switch b := body.(type) {
			case *intents.TnTEventBody:
				logEvent(log, "event published", ref, b)
			case *intents.TnTAlertBody:
				logAlert(log, "alert published", ref, b)
			}
		},
	}
	deny := map[string]bool{}
	for _, d := range o.Deny {
		deny[d] = true
	}
	var model llm.Model
	if cfg.LLM != nil && cfg.LLM.Provider == config.ProviderFake {
		model = operator.FakeInquiryModel()
	}
	handlers[intents.TnTInquiry] = &operator.InquiryService{Bookings: a.Bookings, Log: a.Events, Model: model, Name: cfg.Name,
		OnModelCall: func(c operator.ModelCall) {
			level := slog.LevelInfo
			if c.Outcome != "ok" {
				level = slog.LevelWarn
			}
			log.Log(context.Background(), level, "model call", "booking", c.Booking, "attempt", c.Attempt, "model", c.Model,
				"promptHash", c.PromptHash, "inputTokens", c.Usage.InputTokens, "outputTokens", c.Usage.OutputTokens,
				"latency", c.Latency.Round(time.Millisecond).String(), "outcome", c.Outcome, "reason", c.Reason)
		},
	}
	handlers[intents.BookingRequest] = &operator.BookingService{
		Bookings: a.Bookings, Deny: deny, RefPrefix: o.RefPrefix, ContainerPrefix: o.ContainerPrefix, Now: time.Now,
		Voyage: operator.Voyage{Vessel: o.Vessel, Number: o.Voyage, DepartureHour: o.DepartureHour, Transit: o.Transit},
		OnBooked: func(b store.Booking) {
			if err := a.Simulator.Start(b); err != nil {
				log.Warn("no voyage for booking", "booking", b.Ref, "err", err)
				return
			}
			log.Info("voyage started", "booking", b.Ref, "departure", b.Confirmation.PlannedDeparture,
				"arrival", b.Confirmation.PlannedArrival, "hoursPerSecond", cfg.Simulator.HoursPerSecond)
		},
	}
}

// logEvent logs a validated event's fields: codes from fixed sets, and
// text the intent's rules already cleaned.
func logEvent(log *slog.Logger, msg, ref string, e *intents.TnTEventBody) {
	attrs := []any{"booking", ref, "eventId", e.EventID, "event", e.EventType + " " + e.EventCode + " " + e.EventClassifierCode,
		"at", e.EventDateTime, "location", e.Location.UNLocode}
	if e.EquipmentRef != "" {
		attrs = append(attrs, "container", e.EquipmentRef)
	}
	if e.Transport != nil {
		attrs = append(attrs, "vessel", e.Transport.Vessel+" "+e.Transport.Voyage)
	}
	log.Info(msg, attrs...)
}

// logAlert logs a validated alert, at Warn: it is news.
func logAlert(log *slog.Logger, msg, ref string, al *intents.TnTAlertBody) {
	log.Warn(msg, "booking", ref, "alertId", al.AlertID, "kind", al.Kind, "severity", al.Severity,
		"summary", al.Summary, "previousEta", al.PreviousETA, "newEta", al.NewETA, "causeEventIds", al.CauseEventIDs)
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
