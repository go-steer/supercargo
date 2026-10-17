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
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/supercargo/internal/activity"
	"github.com/go-steer/supercargo/internal/assistant"
	"github.com/go-steer/supercargo/internal/backend"
	"github.com/go-steer/supercargo/internal/business"
	"github.com/go-steer/supercargo/internal/config"
	"github.com/go-steer/supercargo/internal/control"
	"github.com/go-steer/supercargo/internal/docks"
	"github.com/go-steer/supercargo/internal/edi"
	"github.com/go-steer/supercargo/internal/forwarder"
	"github.com/go-steer/supercargo/internal/googleauth"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/anthropic"
	"github.com/go-steer/supercargo/internal/llm/gemini"
	"github.com/go-steer/supercargo/internal/mcpserver"
	"github.com/go-steer/supercargo/internal/node"
	"github.com/go-steer/supercargo/internal/operator"
	"github.com/go-steer/supercargo/internal/persist"
	"github.com/go-steer/supercargo/internal/qualp"
	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/internal/skills"
	"github.com/go-steer/supercargo/internal/store"
	"github.com/go-steer/supercargo/internal/telemetry"
	"github.com/go-steer/supercargo/internal/webui"
	"github.com/go-steer/supercargo/pkg/delegation"
	"github.com/go-steer/supercargo/pkg/discovery"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
	"github.com/go-steer/supercargo/pkg/sig"
	"github.com/go-steer/supercargo/pkg/transport/a2a"
	"github.com/go-steer/supercargo/pkg/transport/rest"
)

// App is a configured node and what it needs to run.
type App struct {
	Config *config.Config
	Node   *node.Node
	// Shipper is set for the shipper or receiver role (a receiver's can't
	// book).
	Shipper *shipper.Shipper
	// Carrier is the operator role's records: the bookings it confirmed and
	// the events and alerts it published. Nil without the operator role.
	Carrier *Records
	// DB is the node's persistence file (config persistence), or nil.
	DB *persist.DB
	// Replay is the node's replay window.
	Replay *store.Replay
	// Customer is the shipper and receiver roles' records: the bookings
	// other carriers issued and the events and alerts received for them.
	// Nil without either role. Nothing reads across Carrier and Customer
	// (docs/DESIGN.md, A.6).
	Customer *Records
	// Keys is the key cache (partners pinned).
	Keys *discovery.Cache
	// Messages is the message log: the envelopes this node sent and
	// received, exactly as on the wire, each with its side. Served on the
	// control API at /messages.
	Messages *store.Messages
	Server   *http.Server
	Logger   *slog.Logger
	// Business is the shipper's private business context (nil if none).
	Business *business.Context
	// Docks is the receiving site's dock schedule (the shipper's own).
	Docks backend.DockAppointments
	// Activity is the node's activity stream: the log's source, served live
	// on the control API at /events.
	Activity *activity.Bus
	// Summarizer writes the shipper's alert notifications.
	Summarizer *shipper.Summarizer

	// controlAPI is the control API's handler: served on the socket, and
	// behind the browser UI's sign-in.
	controlAPI http.Handler
	// mcp is the MCP endpoint, if configured, and inbox the tools behind
	// it, whose drafts people confirm in the browser UI.
	mcp   http.Handler
	inbox *assistant.Assistant
	// mcpMeta is the MCP endpoint's protected-resource metadata, with
	// Google sign-in.
	mcpMeta http.Handler
	// google is the company's Google OAuth client, if configured.
	google *googleauth.Client

	metrics *telemetry.Metrics
	gauges  metric.Registration

	// The operator's push path and simulator, and the presenter's control
	// API (nil unless configured).
	Publisher *operator.Publisher
	Simulator *operator.Simulator
	Control   *http.Server
	// grants takes the delegations other companies hand this node.
	grants *shipper.Grants
	// Orders is a forwarder's order desk, and Orchestrator plans and books
	// its orders' legs (nil unless configured).
	Orders       *forwarder.Orders
	Orchestrator *forwarder.Orchestrator
	Relay        *forwarder.Relay
	// Gateway is a forwarder's EDI desk, for carriers without a node (nil
	// without EDI partners); its file drops close at shutdown.
	Gateway     *forwarder.Gateway
	ediDrops    []*edi.Dir
	stopGateway func()
}

// New builds an App from cfg.
func New(cfg *config.Config, logger *slog.Logger) (_ *App, failed error) {
	var (
		priv ed25519.PrivateKey
		err  error
	)
	if env := cfg.Keys[0].PrivateKeyEnv; env != "" {
		priv, err = config.LoadSigningKeyEnv(env)
	} else {
		priv, err = config.LoadSigningKey(cfg.Keys[0].PrivateKeyFile)
	}
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
	clientOpts := rest.ClientOptions{Peers: cfg.Peers, AllowHTTP: insecure, TLSConfig: clientTLS, MaxTimeout: time.Minute,
		Header: injectTrace}
	client, err := rest.NewClient(cache, clientOpts)
	if err != nil {
		return nil, err
	}
	a2aClient, err := a2a.NewClient(cache, clientOpts)
	if err != nil {
		return nil, err
	}

	a := &App{Config: cfg, Keys: cache, Logger: logger, Activity: activity.NewBus(0), Messages: store.NewMessages(0, 0)}
	if cfg.Has(config.RoleOperator) {
		a.Carrier = newRecords()
	}
	if cfg.Customer() {
		a.Customer = newRecords()
	}
	replay := store.NewReplay(store.ReplayOptions{Retention: cfg.Security.ReplayRetention})
	a.Replay = replay
	if p := cfg.Persistence; p != nil {
		// Restored before anything runs: the handlers number on after what
		// was booked, and a message seen before the restart is still a
		// replay.
		if a.DB, err = persist.Open(p.File); err != nil {
			return nil, err
		}
		defer func() {
			if failed != nil {
				_ = a.DB.Close() // New failed: nothing holds the file
			}
		}()
		if err := a.persist(replay); err != nil {
			return nil, err
		}
	}
	// The log is the activity stream's first reader: what the log says and
	// what a live client sees are the same entries.
	a.Activity.Sink(activity.SlogSink(logger))
	// So are the metrics (recorded only when telemetry is set up).
	metrics, err := telemetry.NewMetrics(otel.Meter("github.com/go-steer/supercargo/internal/app"), cfg.Partners)
	if err != nil {
		return nil, err
	}
	a.Activity.Sink(metrics.Observe)
	a.metrics = metrics
	// Each role brings its own handlers; two never claim one intent.
	handlers := map[string]node.Handler{}
	if cfg.Has(config.RoleOperator) {
		role := map[string]node.Handler{}
		if err := a.operatorRole(role); err != nil {
			return nil, err
		}
		if err := merge(handlers, role); err != nil {
			return nil, err
		}
	}
	// The address book and the directory are read again for each
	// assistant; a broken one stops the node now, not the first chat.
	if _, _, err := Partners(cfg); err != nil {
		return nil, err
	}
	var guard func(string, []byte) error
	if cfg.Business != nil {
		if a.Business, err = business.Load(cfg.Business.File); err != nil {
			return nil, err
		}
		guard = customerGuard(a.Business.Guard())
	}
	if d := cfg.Docks; d != nil {
		zone, _ := time.LoadLocation(d.Zone) // config checked it
		m := &docks.Mock{Source: d.Mock, Site: d.Site, Docks: d.Docks, Zone: zone, Now: time.Now}
		if err := m.Seed(a.Business); err != nil {
			return nil, err
		}
		a.Docks = m
	}
	if cfg.Customer() {
		role := map[string]node.Handler{}
		if err := a.shipperRole(role); err != nil {
			return nil, err
		}
		if err := merge(handlers, role); err != nil {
			return nil, err
		}
	}
	restURL, a2aURL := RESTURL(cfg), ""
	if cfg.Has(config.RoleOperator) {
		// The operator's agent serves the shipment-inquiry skill.
		a2aURL = "https://" + cfg.Domain + "/a2a"
		if insecure {
			a2aURL = "http://" + cfg.Domain + "/a2a"
		}
	}
	n, err := node.New(node.Config{
		Domain: cfg.Domain, Name: cfg.Name, Roles: cfg.Roles,
		KID: kid, Signer: signer, PublicJWK: jwk, RESTURL: restURL, A2AURL: a2aURL, AllowHTTP: insecure,
		Keys:   cache,
		Replay: replay,
		// Whatever the replay window lost in a restart, nothing sent before
		// this start is admitted.
		Since:      time.Now().Truncate(time.Second), // sentAt is in whole seconds
		TrustTrace: trustTrace(cfg.Telemetry),
		MaxSkew:    cfg.Security.MaxSkew,
		Handlers:   handlers,
		Poster:     client,
		A2A:        a2aClient,
		// Defence in depth for the shipper's private business context:
		// nothing builds an envelope from it, and this refuses any that
		// would carry its text.
		OutboundGuard: guard,
		Observer:      a.observe,
	})
	if err != nil {
		return nil, err
	}
	a.Node = n
	if a.Publisher != nil {
		a.Publisher.Pusher = n
	}
	if a.Customer != nil {
		a.Shipper = &shipper.Shipper{Node: n, Bookings: a.Customer.Bookings, Events: a.Customer.Events, Now: time.Now,
			NoBooking: !cfg.Has(config.RoleShipper), Endpoint: RESTURL(cfg)}
		if a.grants != nil {
			a.grants.Shipper = a.Shipper
		}
		if a.DB != nil {
			a.Shipper.OnSaveError = func(err error) { a.Logger.Error("shipper state not saved", "error", err) }
			if err := a.Shipper.PersistSent(a.DB.Bucket("shipper.sent")); err != nil {
				return nil, err
			}
		}
	}
	if a.Orders != nil {
		a.Orders.Node = n
		a.Shipper.InOrder = a.Orders.HoldsLeg
		// The legs are booked through the shipper side, which a forwarder
		// has (config requires it).
		a.Orchestrator = &forwarder.Orchestrator{Orders: a.Orders, Legs: a.Shipper, Cancel: a.Shipper, Lanes: cfg.Forwarder.Lanes,
			Name: cfg.Name, Endpoint: RESTURL(cfg), Now: time.Now}
		if fs := cfg.Forwarder.Freight; fs != nil && fs.QualP != nil {
			// The token from the environment, never the config file.
			token := os.Getenv(fs.QualP.TokenEnv)
			if token == "" {
				return nil, fmt.Errorf("forwarder: freight: qualp: $%s is empty", fs.QualP.TokenEnv)
			}
			label := fs.QualP.Label
			if label == "" {
				label = "QualP"
			}
			a.Orchestrator.Floors = &qualp.Client{BaseURL: fs.QualP.URL, Token: token, Source: label}
		}
		// The legs' events, re-issued to the orders' shippers; a delay's
		// proposals wait for a person.
		a.Relay = &forwarder.Relay{Orders: a.Orders, Publish: a.Publisher, Amend: a.Shipper, Now: time.Now,
			OnProposal: func(p forwarder.Proposal) {
				a.Logger.Info("proposal", "id", p.ID, "order", p.OrderRef, "leg", p.Leg, "why", p.Why)
			}}
		if a.DB != nil {
			a.Relay.OnSaveError = func(err error) { a.Logger.Error("relay state not saved", "error", err) }
			if err := a.Relay.Persist(forwarder.RelaySavers{Statuses: a.DB.Bucket("relay.statuses"), Legs: a.DB.Bucket("relay.legs"),
				Relayed: a.DB.Bucket("relay.relayed"), Proposals: a.DB.Bucket("relay.proposals")}); err != nil {
				return nil, err
			}
		}
		a.Relay.Resume(a.Carrier.Events.IDs()) // restored after a restart: number on from them
		if err := a.ediGateway(); err != nil {
			a.closeDrops()
			return nil, err
		}
	}
	{
		// One control API for every role; startup fails on a route two
		// roles both claim. The socket serves it to the CLIs; the browser
		// UI reaches it only through its own sign-in and side filters.
		mux := http.NewServeMux()
		mux.Handle("GET /events", activity.Handler(a.Activity))
		mux.Handle("GET /messages", a.messagesHandler())
		mux.Handle("GET /messages/{seq}", a.messageHandler())
		var roles []func(*http.ServeMux)
		if a.Carrier != nil {
			roles = append(roles, func(m *http.ServeMux) {
				operator.RegisterControl(m, a.Simulator, a.Carrier.Bookings, a.Carrier.Events)
				// The backend API: the carrier's own systems report events
				// here, as the simulator does for the demo.
				operator.RegisterBackend(m, a.Publisher, a.Carrier.Bookings, a.Carrier.Events,
					func(ref string, body intents.Body) { a.record(activity.ActionPublished, ref, body) })
			})
		}
		if a.Shipper != nil {
			roles = append(roles, func(m *http.ServeMux) { shipper.RegisterControl(m, a.Shipper, cache) })
		}
		if a.Docks != nil {
			roles = append(roles, func(m *http.ServeMux) { docks.RegisterControl(m, a.Docks) })
		}
		if a.Orders != nil {
			roles = append(roles, func(m *http.ServeMux) {
				forwarder.RegisterControl(m, a.Orders, a.Orchestrator, a.Relay, a.Carrier.Events)
			})
		}
		if err := registerRoutes(mux, roles...); err != nil {
			a.closeDrops()
			return nil, err
		}
		a.controlAPI = mux
		if cfg.Control != nil {
			a.Control = control.Server(mux, slog.NewLogLogger(logger.Handler(), slog.LevelInfo))
		}
	}
	if a.google, err = googleClient(cfg); err != nil {
		a.closeDrops()
		return nil, err
	}
	if err := a.mcpServer(); err != nil {
		a.closeDrops()
		return nil, err
	}
	a.Server = &http.Server{
		Addr:    cfg.Listen,
		Handler: a.publicHandler(n),
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

// customerSends are the intents the shipper and receiver roles send: what
// the business context's guard checks, and what puts a conversation on the
// customer side.
var customerSends = map[string]bool{intents.BookingRequest: true, intents.TnTInquiry: true, intents.OceanTnTRequest: true,
	intents.RoadBookingRequest: true, intents.RoadBookingAmendment: true, intents.ForwardingRequest: true, intents.ForwardingStatusRequest: true,
	intents.BookingCancellation: true}

// customerGuard applies the business context's guard to what the customer
// side sends, and only that. Applied to a carrier role's replies, which
// echo the request (an eBL names the consignee), it would let anyone who
// can book test guesses at the private context by whether the reply fails
// (docs/DESIGN.md, A.6, rule 3).
func customerGuard(g func([]byte) error) func(string, []byte) error {
	return func(intent string, body []byte) error {
		if !customerSends[intent] {
			return nil
		}
		return g(body)
	}
}

// publicHandler is what the node's listener serves: the protocol
// endpoints, and the browser UI under /ui/ if the config has a web
// section. One listener, as a Cloud Run service has one port.
func (a *App) publicHandler(n *node.Node) http.Handler {
	protocol := extractTrace(n.Handler())
	if a.mcp != nil {
		// /mcp exactly; everything else reaches the protocol as it came
		// (a ServeMux would clean paths and redirect).
		next := protocol
		protocol = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == mcpserver.Path:
				a.mcp.ServeHTTP(w, r)
			case a.mcpMeta != nil && (r.URL.Path == mcpserver.MetadataPath+mcpserver.Path || r.URL.Path == mcpserver.MetadataPath):
				a.mcpMeta.ServeHTTP(w, r)
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
	w := a.Config.Web
	if w == nil {
		return protocol
	}
	mux := http.NewServeMux()
	mux.Handle("/", protocol)
	personas := make([]webui.Persona, len(w.Personas))
	for i, p := range w.Personas {
		personas[i] = webui.Persona{ID: p.ID, Name: p.Name, Title: p.Title, Roles: p.Roles, Members: p.Members}
	}
	// One assistant per role the node has (with a model): the shipper's on
	// the customer side, the carrier's on the operator side (#75).
	var newAssistant func(side string) (*assistant.Assistant, error)
	var assistantSides []string
	if a.Config.LLM != nil {
		client := control.InProcess(a.controlAPI, 2*time.Minute)
		if a.Config.Customer() {
			// A receiver's too: it follows bookings it's granted and asks
			// about them (its node refuses a booking).
			assistantSides = append(assistantSides, activity.SideCustomer)
		}
		if a.Config.Has(config.RoleOperator) {
			assistantSides = append(assistantSides, activity.SideOperator)
		}
		newAssistant = func(side string) (*assistant.Assistant, error) {
			var as *assistant.Assistant
			var err error
			switch side {
			case activity.SideCustomer:
				as, err = ShipperAssistant(a.Config, assistant.ControlNode{Client: client})
			case activity.SideOperator:
				as, err = CarrierAssistant(a.Config, assistant.ControlCarrierNode{Client: client})
			default:
				err = fmt.Errorf("no assistant for side %q", side)
			}
			if err != nil {
				return nil, err
			}
			// Each model call goes to the node's activity, as the other
			// model roles' do: what was called and how it went, never what
			// was said (the stream is everyone's on this side).
			as.OnModelCall = func(m activity.Model) {
				if m.Agent == "" {
					m.Agent = "assistant"
				}
				a.Activity.Publish(activity.Entry{Side: side, Kind: activity.KindModel, Model: &m})
			}
			return as, nil
		}
	}
	var panels []webui.Panel
	if w.Presenter != nil {
		for _, p := range w.Presenter.Panels {
			panels = append(panels, webui.Panel{Name: p.Name, URL: p.URL})
		}
	}
	mux.Handle(webui.Prefix, webui.Handler(webui.Config{Company: a.Config.Name, Domain: a.Config.Domain, Brand: w.Brand, Personas: personas, NewAssistant: newAssistant,
		AssistantSides: assistantSides, FrameAncestors: w.FrameAncestors, Panels: panels, Inbox: a.inbox, Orders: a.Orders != nil, Google: a.webGoogle(), Control: a.controlAPI,
		Secure: a.Config.TLS.Mode != config.TLSInsecureDev || strings.HasPrefix(publicURL(a.Config), "https://"),
		Hosts:  slices.Concat(listenHosts(a.Config.Listen), publicHost(a.Config), w.Hosts)}))
	return mux
}

// mcpServer builds the MCP endpoint, if configured: the shipper's tools for
// the company's own agents, whose drafts wait in the inbox for a person to
// confirm in the browser UI. A missing or short token stops the node.
func (a *App) mcpServer() error {
	if a.Config.MCP == nil || a.Config.MCP.Server == nil {
		return nil
	}
	ms := a.Config.MCP.Server
	inbox, err := ToolAssistant(a.Config, assistant.ControlNode{Client: control.InProcess(a.controlAPI, 2*time.Minute)})
	if err != nil {
		return err
	}
	// A tag per start: a stale link to an earlier start's draft never opens
	// another draft that got the same number.
	var tag [2]byte
	_, _ = rand.Read(tag[:])
	inbox.Drafts.Prefix = fmt.Sprintf("MD%X", tag)
	verify, meta := a.mcpGoogle()
	token, from := "", "Google sign-in"
	if verify == nil {
		if token, from, err = mcpToken(ms); err != nil {
			return err
		}
	}
	uiURL := ms.UIURL
	if uiURL == "" && a.Config.Web != nil && publicURL(a.Config) != "" {
		uiURL = publicURL(a.Config) + webui.Prefix
	}
	h, err := mcpserver.Handler(mcpserver.Config{Assistant: inbox, Token: token, Verify: verify, Metadata: meta, Name: a.Config.Domain, Version: envelope.Version,
		UIURL: uiURL,
		OnCall: func(t activity.Tool) {
			a.Activity.Publish(activity.Entry{Side: activity.SideCustomer, Kind: activity.KindTool, Tool: &t})
		}})
	if err != nil {
		return fmt.Errorf("mcp: server (token from %s): %w", from, err)
	}
	a.mcp, a.inbox = h, inbox
	if meta != nil {
		a.mcpMeta = mcpserver.MetadataHandler(meta)
	}
	return nil
}

// mcpToken reads the MCP endpoint's token from its environment variable or
// its file, which must be its owner's alone.
func mcpToken(ms *config.MCPServer) (token, from string, err error) {
	token, from, err = secretFrom(ms.TokenEnv, ms.TokenFile)
	if err != nil {
		return "", "", fmt.Errorf("mcp: server: %w", err)
	}
	return token, from, nil
}

// listenHosts is the listen address's host, if it names one, for the UI's
// host check.
func listenHosts(listen string) []string {
	if h, _, err := net.SplitHostPort(listen); err == nil && h != "" {
		return []string{strings.ToLower(h)}
	}
	return nil
}

// registerRoutes adds each role's control routes to mux. ServeMux panics
// on a pattern that conflicts with one already registered; that becomes
// an error here.
func registerRoutes(mux *http.ServeMux, roles ...func(*http.ServeMux)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("app: two roles' control routes collide: %v", r)
		}
	}()
	for _, register := range roles {
		register(mux)
	}
	return nil
}

// Records are one side's bookings and the events and alerts for them.
type Records struct {
	Bookings *store.Bookings
	Events   *store.EventLog
}

func newRecords() *Records {
	return &Records{Bookings: store.NewBookings(), Events: store.NewEventLog(0)}
}

// persist restores each side's records and the replay window from the
// node's file, and saves them there from now on.
func (a *App) persist(replay *store.Replay) error {
	for _, side := range []struct {
		name string
		r    *Records
	}{{"carrier", a.Carrier}, {"customer", a.Customer}} {
		if side.r == nil {
			continue
		}
		if err := side.r.Bookings.Persist(a.DB.Bucket(side.name + ".bookings")); err != nil {
			return err
		}
		if err := side.r.Events.Persist(a.DB.Bucket(side.name + ".events")); err != nil {
			return err
		}
	}
	return replay.Persist(a.DB.Bucket("replay"))
}

// merge adds a role's handlers to the node's, refusing an intent two
// roles both claim.
func merge(into, role map[string]node.Handler) error {
	for intent, h := range role {
		if _, dup := into[intent]; dup {
			return fmt.Errorf("app: two roles handle %s", intent)
		}
		into[intent] = h
	}
	return nil
}

// Serve runs the HTTP server until ctx is done, then shuts it down
// gracefully.
func (a *App) Serve(ctx context.Context) error {
	cfg := a.Config
	a.Logger.Info("node starting", "domain", cfg.Domain, "roles", cfg.Roles, "listen", cfg.Listen, "tls", cfg.TLS.Mode)
	if cfg.TLS.Mode == config.TLSInsecureDev {
		a.Logger.Warn("INSECURE DEVELOPMENT MODE: no TLS. Nothing ties a profile, or its keys, to its domain. Never use this outside a local demo.")
	}
	if cfg.TLS.Mode == config.TLSProxy {
		a.Logger.Info("plain HTTP on the listener: the proxy in front must serve https at the domain", "domain", cfg.Domain)
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
	if a.Gateway != nil {
		gctx, stop := context.WithCancel(context.Background())
		a.stopGateway = stop // shutdown stops it before closing its drops (Close waits for a Step under way)
		go a.Gateway.Run(gctx, 500*time.Millisecond)
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
	if a.stopGateway != nil {
		a.stopGateway()
	}
	a.closeDrops()
	if a.Publisher != nil {
		a.Publisher.Close()
	}
	// End the activity streams: http.Server.Shutdown waits for every
	// connection to go idle, and a stream never would.
	if a.gauges != nil {
		_ = a.gauges.Unregister()
	}
	if a.Summarizer != nil {
		// Before the bus: what is still queued is summarized from the
		// template and logged.
		a.Summarizer.Close()
	}
	a.Activity.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if a.Control != nil {
		_ = a.Control.Shutdown(ctx)
	}
	_ = a.Server.Shutdown(ctx)
	if a.DB != nil {
		_ = a.DB.Close()
	}
}

// operatorRole sets up the booking service, the publisher and simulator
// behind it. The publisher's Pusher is the node, set once it exists.
func (a *App) operatorRole(handlers map[string]node.Handler) error {
	cfg, log := a.Config, a.Logger
	o := cfg.Operator
	a.Publisher = &operator.Publisher{Domain: cfg.Domain, Log: a.Carrier.Events, Now: time.Now,
		OnDrop: func(ref, id string, err error) {
			a.Activity.Publish(activity.Entry{Side: activity.SideOperator, Kind: activity.KindPush,
				Push: &activity.Push{Booking: ref, ID: id, Problem: "dropped", Error: err.Error()}})
		},
		OnRetry:  func(string, string) { a.metrics.Retry() },
		Bookings: a.Carrier.Bookings,
	}
	a.Simulator = &operator.Simulator{Publisher: a.Publisher, HoursPerSecond: cfg.Simulator.HoursPerSecond, Shared: cfg.Simulator.Clock == config.ClockShared,
		OnError: func(ref string, err error) {
			a.Activity.Publish(activity.Entry{Side: activity.SideOperator, Kind: activity.KindPush,
				Push: &activity.Push{Booking: ref, Problem: "unpublished", Error: err.Error()}})
		},
		OnPublish: func(ref string, body intents.Body) { a.record(activity.ActionPublished, ref, body) },
	}
	a.Simulator.Resume(a.Carrier.Events.IDs()) // restored after a restart: number on from them
	reg, err := telemetry.Gauges(otel.Meter("github.com/go-steer/supercargo/internal/app"), a.Publisher.Pending, a.Simulator.Active)
	if err != nil {
		return err
	}
	a.gauges = reg
	deny := map[string]bool{}
	if o != nil {
		for _, d := range o.Deny {
			deny[d] = true
		}
	}
	model, err := NewModel(cfg.LLM, operator.FakeInquiryModel())
	if err != nil {
		return err
	}
	agentSkills, err := agentSkills("shipment-inquiry", cfg.Skills)
	if err != nil {
		return err
	}
	if model != nil {
		log.Info("inquiry responder", "model", model.Name(), "skills", skillIDs(agentSkills))
	}
	handlers[intents.OceanTnTRequest] = &operator.StatusService{Bookings: a.Carrier.Bookings, Log: a.Carrier.Events}
	// A company the booking's shipper delegated it to subscribes to its
	// events (milestone 7).
	handlers[intents.TnTSubscriptionRequest] = &operator.SubscriptionService{Bookings: a.Carrier.Bookings, Now: time.Now,
		OnSubscribed: func(b store.Booking, d store.Delegate) {
			a.Logger.Info("delegated subscription", "booking", b.Ref, "grantee", d.Grantee, "granted_by", b.Shipper, "token", d.TokenID, "until", d.Expires)
		}}
	handlers[intents.TnTInquiry] = &operator.InquiryService{Bookings: a.Carrier.Bookings, Log: a.Carrier.Events, Model: model, Name: cfg.Name, Skills: agentSkills,
		// A forwarder's relay (made later, before the node serves) knows each
		// re-issued record's leg.
		LegOf: func(id string) (int, bool) {
			if a.Relay == nil {
				return 0, false
			}
			return a.Relay.LegOf(id)
		},
		OnModelCall: func(c operator.ModelCall) {
			a.Activity.Publish(activity.Entry{Side: activity.SideOperator, Kind: activity.KindModel, Model: &activity.Model{
				Agent: "inquiry responder", Booking: c.Booking, Attempt: c.Attempt, Model: c.Model, PromptHash: c.PromptHash, Skills: c.Skills,
				InputTokens: c.Usage.InputTokens, OutputTokens: c.Usage.OutputTokens,
				LatencyMS: c.Latency.Round(time.Millisecond).Milliseconds(), Outcome: c.Outcome, Reason: c.Reason}})
		},
	}
	if f := cfg.Forwarder; f != nil {
		fdeny := map[string]bool{}
		for _, d := range f.Deny {
			fdeny[d] = true
		}
		// The node is set once it exists (New), like the publisher's.
		a.Orders = &forwarder.Orders{Bookings: a.Carrier.Bookings, RefPrefix: f.RefPrefix, Deny: fdeny, Now: time.Now,
			OnOrder:  func(b store.Booking) { a.orderEntry(b, "taken") },
			OnAnswer: func(b store.Booking) { a.orderEntry(b, "answered") },
		}
		handlers[intents.ForwardingRequest] = a.Orders
		handlers[intents.ForwardingStatusRequest] = &forwarder.Status{Orders: a.Orders, Log: a.Carrier.Events}
	}
	if o == nil {
		return nil // a forwarder only: it carries nothing itself
	}
	handlers[intents.BookingCancellation] = &operator.CancellationService{Bookings: a.Carrier.Bookings, Log: a.Carrier.Events, Now: time.Now,
		Stop: a.Simulator.Cancel, Exclusive: a.Publisher.Exclusive,
		OnCancelled: func(b store.Booking) {
			a.Activity.Publish(activity.Entry{Side: activity.SideOperator, Kind: activity.KindVoyage,
				Voyage: &activity.Voyage{Booking: b.Ref, Cancelled: true}})
		}}
	if r := o.Road; r != nil {
		// A road carrier: its booking system plans each leg (in memory
		// until a backend adapter is configured).
		prefix := o.RefPrefix
		if o.Ocean() {
			// The ocean bookings number from the same prefix in the same
			// store: the truck legs get their own, so no two collide.
			prefix += "-R"
		}
		trucks := &operator.TruckBookings{RefPrefix: prefix, Transit: r.Transit, Lead: r.Lead, ECMRPrefix: r.ECMRPrefix, Now: time.Now}
		if r.CTe != nil {
			trucks.CTe = &operator.CTeIssuer{CNPJ: r.CTe.CNPJ, State: r.CTe.State, MDFe: r.CTe.MDFe}
		}
		trucks.TMS = r.TMS
		if r.Risk != nil {
			trucks.Risk = &operator.MockRisk{Source: r.Risk.Mock}
		}
		if r.TollVoucher != nil {
			trucks.Vouchers = &operator.MockVouchers{Source: r.TollVoucher.Mock}
		}
		if r.Planning != nil {
			p, err := planning(r.Planning)
			if err != nil {
				return err
			}
			// Why a service didn't answer, for the carrier's operators (the
			// step says only "not available").
			p.OnError = func(system string, err error) { a.Logger.Warn("journey planning", "system", system, "error", err) }
			trucks.Planning = p
		}
		if r.Curfew != nil {
			a.curfew(r.Curfew, r.DriverApp)
		}
		trucks.Resume(a.Carrier.Bookings.List()) // restored after a restart: number on from them
		// Each leg plays out on the simulator, as a voyage does; an
		// amendment re-plans it.
		leg := func(b store.Booking, err error) {
			c := b.Road.Confirmation
			v := &activity.Voyage{Booking: b.Ref, Started: true, Departure: c.PlannedPickup, Arrival: c.PlannedDelivery, HoursPerSecond: cfg.Simulator.HoursPerSecond}
			if err != nil {
				v = &activity.Voyage{Booking: b.Ref, Error: err.Error()}
			}
			a.Activity.Publish(activity.Entry{Side: activity.SideOperator, Kind: activity.KindVoyage, Voyage: v})
		}
		handlers[intents.RoadBookingRequest] = &operator.RoadBookingService{Bookings: a.Carrier.Bookings, Deny: deny, Backend: trucks, Now: time.Now,
			OnBooked: func(b store.Booking) { leg(b, a.Simulator.Start(b)) }}
		handlers[intents.RoadBookingAmendment] = &operator.RoadAmendmentService{Bookings: a.Carrier.Bookings, Backend: trucks,
			OnAmended: func(b store.Booking) { leg(b, a.Simulator.Replan(b)) }}
	}
	if !o.Ocean() {
		return nil
	}
	handlers[intents.BookingRequest] = &operator.BookingService{
		Bookings: a.Carrier.Bookings, Deny: deny, RefPrefix: o.RefPrefix, ContainerPrefix: o.ContainerPrefix, Now: time.Now,
		Voyage: operator.Voyage{Vessel: o.Vessel, Number: o.Voyage, DepartureHour: o.DepartureHour, Transit: o.Transit, Transits: o.Transits},
		OnBooked: func(b store.Booking) {
			v := &activity.Voyage{Booking: b.Ref, Started: true, Departure: b.Confirmation.PlannedDeparture,
				Arrival: b.Confirmation.PlannedArrival, HoursPerSecond: cfg.Simulator.HoursPerSecond}
			if err := a.Simulator.Start(b); err != nil {
				v = &activity.Voyage{Booking: b.Ref, Error: err.Error()}
			}
			a.Activity.Publish(activity.Entry{Side: activity.SideOperator, Kind: activity.KindVoyage, Voyage: v})
		},
	}
	return nil
}

// shipperRole sets up the inbound push handler and the alert summarizer
// behind it.
func (a *App) shipperRole(handlers map[string]node.Handler) error {
	cfg := a.Config
	model, err := NewModel(cfg.LLM, shipper.FakeSummaryModel())
	if err != nil {
		return err
	}
	agentSkills, err := agentSkills("alert-summary", cfg.Skills)
	if err != nil {
		return err
	}
	if model != nil {
		a.Logger.Info("alert summarizer", "model", model.Name(), "skills", skillIDs(agentSkills))
	}
	var lang string
	if cfg.LLM != nil {
		lang = cfg.LLM.Language
	}
	a.Summarizer = &shipper.Summarizer{Log: a.Customer.Events, Business: a.Business, Model: model, Skills: agentSkills, Name: cfg.Name, Language: lang,
		OnSummary: func(s activity.Summary) {
			a.Activity.Publish(activity.Entry{Side: activity.SideCustomer, Kind: activity.KindSummary, Summary: &s})
		},
		OnModelCall: func(m activity.Model) {
			m.Agent = "alert summarizer"
			a.Activity.Publish(activity.Entry{Side: activity.SideCustomer, Kind: activity.KindModel, Model: &m})
		},
	}
	inbound := &shipper.Inbound{Bookings: a.Customer.Bookings, Log: a.Customer.Events, Now: time.Now,
		OnEvent: func(b store.Booking, e *intents.TnTEventBody) {
			a.record(activity.ActionRecorded, b.Ref, e)
			a.relay(func(r *forwarder.Relay) error { return r.Event(b, e) })
		},
		OnAlert: func(b store.Booking, al *intents.TnTAlertBody) {
			a.record(activity.ActionRecorded, b.Ref, al)
			a.Summarizer.Enqueue(b, al)
			a.relay(func(r *forwarder.Relay) error { return r.Alert(b, al) })
		},
	}
	handlers[intents.TnTEvent] = inbound
	handlers[intents.TnTAlert] = inbound
	// A booking another company's shipper granted us (milestone 7): kept,
	// then subscribed to with its carrier, off the request.
	endpoint := RESTURL(cfg)
	from := map[string]bool{}
	if cfg.Grants != nil {
		for _, d := range cfg.Grants.From {
			from[d] = true
		}
	}
	a.grants = &shipper.Grants{Endpoint: endpoint, From: from, OnGranted: func(b store.Booking) {
		a.Logger.Info("delegation received", "booking", b.Ref, "carrier", b.Operator, "granted_by", b.Delegated.Issuer, "until", b.Delegated.Expires)
		if !slices.Contains(b.Delegated.Scope, delegation.ScopeSubscribe) {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), shipper.GrantTimeout)
			defer cancel()
			if c, err := a.Shipper.Subscribe(ctx, b.Operator, b.Ref, endpoint); err != nil {
				a.Logger.Warn("delegated subscription failed", "booking", b.Ref, "carrier", b.Operator, "error", err)
			} else {
				a.Logger.Info("delegated subscription", "booking", b.Ref, "carrier", b.Operator, "status", c.Status)
			}
		}()
	}}
	handlers[intents.DelegationGrant] = a.grants
	// A forwarder's final answer to an order taken as pending.
	handlers[intents.ForwardingConfirmation] = &shipper.OrderAnswers{Bookings: a.Customer.Bookings,
		OnAnswer: func(b store.Booking) { a.orderEntry(b, "answered by its forwarder") }}
	return nil
}

// ediGateway opens a forwarder's EDI file drops and builds its gateway,
// the orchestrator's way to book carriers without a node; their statuses
// go to the relay, re-issued as unsigned.
func (a *App) ediGateway() error {
	cfg := a.Config
	if cfg.Forwarder == nil || len(cfg.Forwarder.EDI) == 0 {
		return nil
	}
	g := &forwarder.Gateway{Self: cfg.SelfEDIID(), Partners: map[string]*forwarder.EDIPartner{}, Status: a.Relay.Unsigned,
		Now: time.Now, Log: a.Logger.With("component", "edi")}
	for _, p := range cfg.Forwarder.EDI {
		zone, err := time.LoadLocation(p.Zone) // validated with the config
		if err != nil {
			return err
		}
		out, err := edi.NewDir(p.Outbox)
		if err != nil {
			return fmt.Errorf("app: %s's EDI outbox: %w", p.Carrier, err)
		}
		a.ediDrops = append(a.ediDrops, out)
		in, err := edi.NewDir(p.Inbox)
		if err != nil {
			return fmt.Errorf("app: %s's EDI inbox: %w", p.Carrier, err)
		}
		a.ediDrops = append(a.ediDrops, in)
		g.Partners[p.Carrier] = &forwarder.EDIPartner{ID: p.ID, Out: out, In: in, Zone: zone, Transit: p.Transit}
	}
	if a.DB != nil {
		if err := g.Persist(a.DB.Bucket("forwarder.edi")); err != nil {
			return err
		}
	}
	a.Gateway, a.Orchestrator.EDI = g, g
	return nil
}

func (a *App) closeDrops() {
	for _, d := range a.ediDrops {
		_ = d.Close()
	}
	a.ediDrops = nil
}

// relay hands a leg's verified event or alert to a forwarder's relay (a
// node without one has nothing to relay). A failure is logged: the leg's
// record stands either way.
func (a *App) relay(f func(*forwarder.Relay) error) {
	if a.Relay == nil {
		return
	}
	if err := f(a.Relay); err != nil {
		a.Logger.Warn("relay", "error", err)
	}
}

// orderEntry logs what happened to a forwarding order. (The activity
// stream gains order entries with the forwarder's views.)
func (a *App) orderEntry(b store.Booking, what string) {
	a.Logger.Info("order "+what, "order", b.Ref, "shipper", b.Shipper, "forwarder", b.Operator, "status", b.Order.Confirmation.Status)
}

func skillIDs(all []skills.Skill) []string {
	var ids []string
	for _, s := range all {
		ids = append(ids, s.ID()+" ("+s.Source+")")
	}
	return ids
}

// record publishes an event or alert recorded or published for ref. The
// bodies were validated (received ones by the pipeline, published ones by
// the publisher), so their text is safe to show.
func (a *App) record(action, ref string, body intents.Body) {
	r := &activity.Record{Action: action, Booking: ref}
	switch b := body.(type) {
	case *intents.TnTEventBody:
		r.Event = b
	case *intents.TnTAlertBody:
		r.Alert = b
	default:
		return
	}
	side := activity.SideCustomer // recorded: received for a customer-side booking
	if action == activity.ActionPublished {
		side = activity.SideOperator
	}
	a.Activity.Publish(activity.Entry{Side: side, Kind: activity.KindRecord, Record: r})
}

// observe publishes the node's decision about an envelope, and keeps the
// envelope itself in the message log. The bytes go to the log only, never
// to the activity stream (or the slog log it feeds).
func (a *App) observe(e node.Event) {
	side := a.sideOf(e.Opener, e.OpenedHere)
	a.Activity.Publish(activity.Entry{Time: e.Time, Side: side, Kind: activity.KindEnvelope, Envelope: &activity.Envelope{
		Direction: e.Direction, Intent: e.Intent, Sender: e.Sender, Receiver: e.Receiver, Via: e.Via,
		MessageID: e.MessageID, KID: e.KID, Verified: e.Verified, Outcome: e.Outcome, Code: e.Code, Reason: e.Reason}})
	if e.Wire == nil {
		return
	}
	m := store.Message{Time: e.Time, Side: side, Direction: e.Direction, Via: e.Via, Intent: e.Intent, Sender: e.Sender,
		Receiver: e.Receiver, MessageID: e.MessageID, KID: e.KID, Verified: e.Verified, Outcome: e.Outcome, Code: e.Code,
		Reason: e.Reason, Header: e.Header, Body: e.Body, Wire: string(e.Wire)}
	if e.Header != nil {
		m.Conversation = e.Header.CorrelationID
	}
	a.Messages.Add(m)
}

// sideOf says which of the node's roles a message belongs to, from the
// intent that opened its conversation and who sent that: requests the
// customer side sends (a booking, an inquiry, a status request) and the
// pushes the operator side sends. A side the node doesn't have (a booking
// request to a shipper-only node) belongs to the whole node: "".
func (a *App) sideOf(opener string, openedHere bool) string {
	var side string
	switch {
	case customerSends[opener] && openedHere, operatorPushes[opener] && !openedHere:
		side = activity.SideCustomer
	case customerSends[opener], operatorPushes[opener]:
		side = activity.SideOperator
	}
	if side == activity.SideCustomer && a.Customer == nil || side == activity.SideOperator && a.Carrier == nil {
		return ""
	}
	return side
}

// operatorPushes are the intents the operator role pushes.
var operatorPushes = map[string]bool{intents.TnTEvent: true, intents.TnTAlert: true, intents.ForwardingConfirmation: true}

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

// RESTURL is where a node from cfg serves the REST binding: where
// partners send, and where a shipper's bookings ask for pushes.
func RESTURL(cfg *config.Config) string {
	if cfg.TLS.Mode == config.TLSInsecureDev {
		return "http://" + cfg.Domain + "/ulp"
	}
	return "https://" + cfg.Domain + "/ulp"
}

// NewModel builds the configured model: none (template output only), the
// caller's fake, or Claude with its key from the environment.
func NewModel(c *config.LLM, fake llm.Model) (llm.Model, error) {
	switch {
	case c == nil:
		return nil, nil
	case c.Provider == config.ProviderFake:
		return fake, nil
	}
	env := func(name, what string) (string, error) {
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("llm: the environment variable %s, which holds the %s, is empty", name, what)
		}
		return v, nil
	}
	if c.Backend == config.BackendVertex {
		// Vertex AI: Application Default Credentials, no key.
		project, err := env(c.ProjectEnv, "Vertex AI project")
		if err != nil {
			return nil, err
		}
		location, err := env(c.LocationEnv, "Vertex AI location")
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c.Provider == config.ProviderGemini {
			if c.Model == "" {
				return nil, errors.New("llm: gemini needs a model")
			}
			return gemini.New(ctx, gemini.Options{Model: c.Model, Vertex: &gemini.Vertex{Project: project, Location: location}, ThinkingBudget: c.ThinkingBudget})
		}
		creds, err := anthropic.DefaultCredentials()
		if err != nil {
			return nil, err
		}
		return anthropic.New(anthropic.Options{Model: c.Model, CacheSystem: true,
			Vertex: &anthropic.Vertex{Project: project, Region: location, Credentials: creds}})
	}
	key, err := env(c.APIKeyEnv, "API key")
	if err != nil {
		return nil, err
	}
	if c.Provider == config.ProviderGemini {
		if c.Model == "" {
			return nil, errors.New("llm: gemini needs a model")
		}
		return gemini.New(context.Background(), gemini.Options{Model: c.Model, APIKey: key, ThinkingBudget: c.ThinkingBudget})
	}
	return anthropic.New(anthropic.Options{APIKey: key, Model: c.Model, CacheSystem: true})
}

// agentSkills are a model's agent skills: the named built-in skill (the
// inquiry responder's or the summarizer's), then the company's own.
func agentSkills(name string, c *config.Skills) ([]skills.Skill, error) {
	builtin, ok := skills.Find(skills.Builtin(), name)
	if !ok {
		panic("app: no built-in skill " + name) // a build defect; the tests catch it
	}
	out := []skills.Skill{builtin}
	if c != nil {
		company, err := skills.LoadDir(c.Dir)
		if err != nil {
			return nil, err
		}
		out = append(out, company...)
	}
	return out, nil
}

// traceContext is the only propagator: W3C traceparent and tracestate.
// Baggage is never sent or accepted.
var traceContext = propagation.TraceContext{}

// injectTrace adds the trace context of an outbound exchange (its
// ulp.egress span) to the request's headers: trace context travels in
// HTTP, never in the signed envelope.
func injectTrace(ctx context.Context, h http.Header) {
	traceContext.Inject(ctx, propagation.HeaderCarrier(h))
}

// extractTrace passes a request's trace context to the pipeline, which
// links to it, or continues it for a trusted peer (node.WithRemoteTrace).
func extractTrace(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") != "" {
			sc := trace.SpanContextFromContext(traceContext.Extract(context.Background(), propagation.HeaderCarrier(r.Header)))
			r = r.WithContext(node.WithRemoteTrace(r.Context(), sc))
		}
		h.ServeHTTP(w, r)
	})
}

// trustTrace is node.Config.TrustTrace from the telemetry config: the
// peers whose trace context is continued.
func trustTrace(t *config.Telemetry) func(string) bool {
	// Only when exporting: otherwise a continued context would just be
	// passed along.
	if t == nil || len(t.TrustPeers) == 0 || !telemetry.Enabled(t, os.Getenv) {
		return nil
	}
	return func(d string) bool { return slices.Contains(t.TrustPeers, d) }
}
