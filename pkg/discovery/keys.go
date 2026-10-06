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

package discovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/internal/logsafe"
	"github.com/go-steer/supercargo/pkg/sig"
)

var (
	// ErrUnknownKey reports a kid that the sender's current profile doesn't
	// publish. It maps to the ulp.error code "unknown_key".
	ErrUnknownKey = errors.New("discovery: unknown key")

	// ErrUnavailable reports a profile that could not be fetched (or was
	// fetched too recently to try again). Envelopes from that domain can't
	// be verified until it can. It maps to "unknown_key", retryable.
	ErrUnavailable = errors.New("discovery: profile unavailable")
)

// Source fetches a domain's profile. *Fetcher implements it.
type Source interface {
	Fetch(ctx context.Context, domain string) (*Document, error)
}

// CacheOptions configures a Cache. Zero or negative durations and counts
// mean the default.
type CacheOptions struct {
	// TTL is how long a fetched profile is used (default 1h). A profile
	// older than this is never used, even if refetching fails: a withdrawn
	// key stops verifying within TTL.
	TTL time.Duration
	// RefreshFloor is the least time between two fetches for one cached
	// domain, successful or not (default 1m). It bounds how often an
	// unknown kid, which anyone can put in an envelope, triggers a fetch.
	RefreshFloor time.Duration
	// MaxDomains bounds the domains cached (default 1024). When the cache is
	// full, a new domain may only replace an entry without a valid profile
	// (failed, never fetched, or expired); if every entry is valid, the new
	// domain's profile is fetched and used but not kept. Sender domains come
	// from unauthenticated envelopes, so junk domains must never push out a
	// partner's valid keys.
	MaxDomains int
	// FetchTimeout bounds one fetch, independently of any one caller's
	// context, since several callers may be waiting on it (default 10s).
	FetchTimeout time.Duration
	// MaxConcurrentFetches bounds fetches in flight for domains that aren't
	// pinned (default 16). A lookup that would exceed it fails with
	// ErrUnavailable rather than queue, so envelopes naming many unknown
	// domains can't turn the node into a source of outbound requests.
	MaxConcurrentFetches int
	// Pinned are domains the node works with by configuration (its
	// partners). They are never evicted, and their fetches don't count
	// against MaxConcurrentFetches, so a flood of unknown domains can
	// neither push out nor starve them.
	Pinned []string
	// Now replaces time.Now (tests).
	Now func() time.Time
}

// Cache resolves (domain, kid) to a verifier, fetching and caching
// profiles. It is safe for concurrent use, and concurrent lookups for one
// domain share one fetch.
type Cache struct {
	src    Source
	opts   CacheOptions
	pinned map[string]bool
	slots  chan struct{} // one token per unpinned fetch in flight

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	doc         *Document
	fetchedAt   time.Time
	lastAttempt time.Time
	lastUsed    time.Time
	inflight    *call
}

// call is one fetch that several lookups wait on. Its fields are written
// before done is closed and read only after.
type call struct {
	done chan struct{}
	doc  *Document
	err  error
}

// NewCache returns a Cache over src. Pinned entries that aren't valid
// domains are an error.
func NewCache(src Source, opts CacheOptions) (*Cache, error) {
	if opts.TTL <= 0 {
		opts.TTL = time.Hour
	}
	if opts.RefreshFloor <= 0 {
		opts.RefreshFloor = time.Minute
	}
	if opts.MaxDomains <= 0 {
		opts.MaxDomains = 1024
	}
	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = 10 * time.Second
	}
	if opts.MaxConcurrentFetches <= 0 {
		opts.MaxConcurrentFetches = 16
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	pinned := map[string]bool{}
	for _, d := range opts.Pinned {
		if !envelope.ValidDomain(d) {
			return nil, fmt.Errorf("discovery: pinned domain %s is not a lowercase domain", logsafe.Quote(d))
		}
		pinned[d] = true
	}
	return &Cache{
		src:     src,
		opts:    opts,
		pinned:  pinned,
		slots:   make(chan struct{}, opts.MaxConcurrentFetches),
		entries: map[string]*entry{},
	}, nil
}

// Key returns the verifier domain publishes under kid. It fetches the
// profile when none is cached, when the cached one is older than TTL, or
// when it lacks kid (a rotated key), but no more often than RefreshFloor.
// Errors wrap ErrUnknownKey or ErrUnavailable. domain must already be
// validated (envelope.Parse does that for a sender).
func (c *Cache) Key(ctx context.Context, domain, kid string) (sig.Verifier, error) {
	doc, fresh, err := c.document(ctx, domain, kid)
	if err != nil {
		return nil, err
	}
	if v, ok := doc.Key(kid); ok {
		return v, nil
	}
	if fresh {
		return nil, fmt.Errorf("%w: %s publishes no key %s", ErrUnknownKey, domain, logsafe.Quote(kid))
	}
	return nil, fmt.Errorf("%w: %s publishes no key %s (and its profile can't be refreshed yet)", ErrUnknownKey, domain, logsafe.Quote(kid))
}

// Profile returns domain's profile, fetching it under the same rules as
// Key. Transports use it to find a partner's endpoints. The Document is
// shared: callers must not modify it.
func (c *Cache) Profile(ctx context.Context, domain string) (*Document, error) {
	doc, _, err := c.document(ctx, domain, "")
	return doc, err
}

// document returns a profile no older than TTL that publishes kid if
// possible. fresh reports that the profile was just fetched, as opposed to
// a cached one that couldn't be refetched because of the floor.
func (c *Cache) document(ctx context.Context, domain, kid string) (doc *Document, fresh bool, err error) {
	now := c.opts.Now()
	c.mu.Lock()
	e := c.entries[domain]
	var cl *call
	if e != nil {
		e.lastUsed = now
		valid := e.doc != nil && now.Sub(e.fetchedAt) < c.opts.TTL
		if valid {
			if _, ok := e.doc.Key(kid); ok || kid == "" {
				doc := e.doc
				c.mu.Unlock()
				return doc, true, nil
			}
		}
		cl = e.inflight
		if cl == nil && now.Sub(e.lastAttempt) < c.opts.RefreshFloor {
			doc := e.doc
			c.mu.Unlock()
			if valid {
				return doc, false, nil // kid absent; Key reports it
			}
			return nil, false, fmt.Errorf("%w: %s (last fetch failed or is too recent to repeat)", ErrUnavailable, domain)
		}
	}
	if cl == nil {
		pinned := c.pinned[domain]
		if !pinned {
			select {
			case c.slots <- struct{}{}:
			default:
				c.mu.Unlock()
				return nil, false, fmt.Errorf("%w: %s (too many profile fetches in flight)", ErrUnavailable, domain)
			}
		}
		if e == nil {
			// A new domain gets an entry only if it costs no partner its
			// valid keys; otherwise this fetch answers its callers and is
			// not kept.
			e = &entry{lastUsed: now}
			if len(c.entries) < c.opts.MaxDomains || pinned || c.evictLocked(now) {
				c.entries[domain] = e
			}
		}
		cl = &call{done: make(chan struct{})}
		e.inflight = cl
		e.lastAttempt = now
		go c.fetch(domain, e, cl, !pinned) //nolint:gosec // G118: deliberate. The fetch is shared by every waiting lookup, so it runs on its own deadline (FetchTimeout), not on one caller's context.
	}
	c.mu.Unlock()

	select {
	case <-cl.done:
	case <-ctx.Done():
		return nil, false, fmt.Errorf("%w: %s: %w", ErrUnavailable, domain, ctx.Err())
	}
	if cl.err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrUnavailable, cl.err)
	}
	return cl.doc, true, nil
}

// fetch runs one fetch on its own deadline, so one impatient caller can't
// fail it for the others waiting.
func (c *Cache) fetch(domain string, e *entry, cl *call, slot bool) {
	// The slot is released before the result is published, so a caller
	// that gets its answer and looks up again never finds this fetch still
	// counted. The deferred release covers a Source that panics.
	release := func() {
		if slot {
			slot = false
			<-c.slots
		}
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.FetchTimeout)
	defer cancel()
	doc, err := c.src.Fetch(ctx, domain)
	if err == nil && doc == nil {
		err = fmt.Errorf("discovery: source returned no profile for %s", domain)
	}
	c.mu.Lock()
	if err == nil {
		e.doc = doc
		e.fetchedAt = c.opts.Now()
	}
	e.inflight = nil
	c.mu.Unlock()
	release()
	cl.doc, cl.err = doc, err
	close(cl.done)
}

// evictLocked makes room by dropping the least recently used entry that
// holds no valid profile and isn't pinned or being fetched. It reports
// whether it found one.
func (c *Cache) evictLocked(now time.Time) bool {
	var victim string
	var victimUsed time.Time
	for d, e := range c.entries {
		if e.inflight != nil || c.pinned[d] {
			continue
		}
		if e.doc != nil && now.Sub(e.fetchedAt) < c.opts.TTL {
			continue // valid: never displaced by a newcomer
		}
		if victim == "" || e.lastUsed.Before(victimUsed) {
			victim, victimUsed = d, e.lastUsed
		}
	}
	if victim == "" {
		return false
	}
	delete(c.entries, victim)
	return true
}
