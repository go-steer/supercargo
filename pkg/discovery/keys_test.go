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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource serves documents per domain and counts fetches. When gate is
// set, fetches block until it is closed.
type fakeSource struct {
	t     *testing.T
	mu    sync.Mutex
	kids  map[string][]string // domain → kids its profile publishes; missing = fetch fails
	gate  chan struct{}
	calls atomic.Int32
}

func (f *fakeSource) Fetch(ctx context.Context, d string) (*Document, error) {
	f.calls.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	kids, ok := f.kids[d]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("connection refused")
	}
	m := profileMap(f.t, kids...)
	m["party"].(map[string]any)["domain"] = d
	for _, k := range []string{"rest", "a2a"} {
		m["transports"].(map[string]any)[k] = "https://" + d + "/" + k
	}
	return ParseProfile(encode(f.t, m), ParseOptions{Domain: d})
}

func (f *fakeSource) set(d string, kids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if kids == nil {
		delete(f.kids, d)
		return
	}
	f.kids[d] = kids
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func newCache(t *testing.T, opts CacheOptions) (*Cache, *fakeSource, *clock) {
	src := &fakeSource{t: t, kids: map[string][]string{domain: {"ob-2026-1"}}}
	clk := &clock{now: time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)}
	opts.Now = clk.Now
	c, err := NewCache(src, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, src, clk
}

func TestCacheDefaults(t *testing.T) {
	src := &fakeSource{t: t, kids: map[string][]string{domain: {"ob-2026-1"}}}
	for _, opts := range []CacheOptions{{}, {TTL: -1, RefreshFloor: -1, MaxDomains: -1, FetchTimeout: -1, MaxConcurrentFetches: -1}} {
		c, err := NewCache(src, opts)
		if err != nil {
			t.Fatal(err)
		}
		if c.opts.TTL != time.Hour || c.opts.RefreshFloor != time.Minute || c.opts.MaxDomains != 1024 ||
			c.opts.FetchTimeout != 10*time.Second || cap(c.slots) != 16 {
			t.Errorf("defaults for %+v: %+v, slots %d", opts, c.opts, cap(c.slots))
		}
		if _, err := c.Key(context.Background(), domain, "ob-2026-1"); err != nil {
			t.Errorf("Key with the real clock: %v", err)
		}
	}
	for _, bad := range []string{"Ocean-Blue.example", "10.0.0.1", ""} {
		if _, err := NewCache(src, CacheOptions{Pinned: []string{bad}}); err == nil {
			t.Errorf("pinned %q accepted", bad)
		}
	}
}

// nilSource returns no profile and no error, which a Source must not do.
type nilSource struct{}

func (nilSource) Fetch(context.Context, string) (*Document, error) { return nil, nil }

func TestCacheNilProfileIsAnError(t *testing.T) {
	c, err := NewCache(nilSource{}, CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Key(context.Background(), domain, "ob-2026-1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Key over a nil profile = %v, want ErrUnavailable", err)
	}
}

// TestCacheKeepsValidProfileWhenRefetchFails: an unknown kid prompts a
// refetch; if that fails, the profile still within TTL keeps verifying its
// known keys.
func TestCacheKeepsValidProfileWhenRefetchFails(t *testing.T) {
	c, src, clk := newCache(t, CacheOptions{RefreshFloor: time.Minute})
	ctx := context.Background()
	if _, err := c.Key(ctx, domain, "ob-2026-1"); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	src.set(domain)
	if _, err := c.Key(ctx, domain, "made-up"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Key(made-up) with fetch failing = %v, want ErrUnavailable", err)
	}
	if _, err := c.Key(ctx, domain, "ob-2026-1"); err != nil {
		t.Errorf("known key after a failed refetch within TTL: %v", err)
	}
}

func TestCacheFetchTimeout(t *testing.T) {
	c, src, _ := newCache(t, CacheOptions{FetchTimeout: 20 * time.Millisecond})
	src.gate = make(chan struct{}) // never opened: the fetch hangs until its deadline
	start := time.Now()
	if _, err := c.Key(context.Background(), domain, "ob-2026-1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Key over a hanging source = %v, want ErrUnavailable", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("FetchTimeout didn't bound the fetch: %v", d)
	}
}

func TestCacheFetchesOnce(t *testing.T) {
	c, src, _ := newCache(t, CacheOptions{})
	for range 3 {
		if v, err := c.Key(context.Background(), domain, "ob-2026-1"); err != nil || v == nil {
			t.Fatalf("Key: %v", err)
		}
	}
	if n := src.calls.Load(); n != 1 {
		t.Errorf("%d fetches, want 1", n)
	}
	if d, err := c.Profile(context.Background(), domain); err != nil || d.Profile.Party.Domain != domain {
		t.Errorf("Profile = %v, %v", d, err)
	}
}

// TestCacheUnknownKidAndRotation: an unknown kid refetches, but at most once
// per floor, so anyone naming made-up kids can't make the node fetch at
// will; a rotated key is picked up once the floor has passed.
func TestCacheUnknownKidAndRotation(t *testing.T) {
	c, src, clk := newCache(t, CacheOptions{RefreshFloor: time.Minute})
	ctx := context.Background()
	if _, err := c.Key(ctx, domain, "ob-2026-1"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := c.Key(ctx, domain, "made-up"); !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("Key(made-up) = %v, want ErrUnknownKey", err)
		}
	}
	if n := src.calls.Load(); n != 1 {
		t.Errorf("unknown kids within the floor caused %d fetches, want 1", n)
	}
	src.set(domain, "ob-2026-1", "ob-2026-2")
	if _, err := c.Key(ctx, domain, "ob-2026-2"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("rotated key found before the floor passed: %v", err)
	}
	clk.advance(time.Minute)
	if _, err := c.Key(ctx, domain, "ob-2026-2"); err != nil {
		t.Errorf("rotated key after the floor: %v", err)
	}
	clk.advance(time.Minute)
	if _, err := c.Key(ctx, domain, "made-up"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Key(made-up) after a refetch = %v, want ErrUnknownKey", err)
	}
	if n := src.calls.Load(); n != 3 {
		t.Errorf("%d fetches, want 3", n)
	}
}

// TestCacheNeverServesExpired: past TTL, a profile is refetched, and if that
// fails the old keys are not used: a withdrawn key stops verifying.
func TestCacheNeverServesExpired(t *testing.T) {
	c, src, clk := newCache(t, CacheOptions{TTL: time.Hour, RefreshFloor: time.Minute})
	ctx := context.Background()
	if _, err := c.Key(ctx, domain, "ob-2026-1"); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)
	src.set(domain) // the domain's server is down
	if _, err := c.Key(ctx, domain, "ob-2026-1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Key after TTL with fetch failing = %v, want ErrUnavailable", err)
	}
	// Retrying within the floor doesn't fetch again.
	if _, err := c.Key(ctx, domain, "ob-2026-1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Key = %v, want ErrUnavailable", err)
	}
	if n := src.calls.Load(); n != 2 {
		t.Errorf("%d fetches, want 2", n)
	}
	clk.advance(time.Minute)
	src.set(domain, "ob-2026-1")
	if _, err := c.Key(ctx, domain, "ob-2026-1"); err != nil {
		t.Errorf("Key after recovery: %v", err)
	}
}

func TestCacheFirstFetchFails(t *testing.T) {
	c, src, clk := newCache(t, CacheOptions{RefreshFloor: time.Minute})
	src.set(domain)
	for range 3 {
		if _, err := c.Key(context.Background(), domain, "ob-2026-1"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Key = %v, want ErrUnavailable", err)
		}
	}
	if n := src.calls.Load(); n != 1 {
		t.Errorf("failures within the floor caused %d fetches, want 1", n)
	}
	clk.advance(time.Minute)
	_, _ = c.Key(context.Background(), domain, "ob-2026-1")
	if n := src.calls.Load(); n != 2 {
		t.Errorf("%d fetches after the floor, want 2", n)
	}
}

func TestCacheSingleFlight(t *testing.T) {
	c, src, _ := newCache(t, CacheOptions{})
	src.gate = make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for range 50 {
		wg.Go(func() {
			_, err := c.Key(context.Background(), domain, "ob-2026-1")
			errs <- err
		})
	}
	waitFor(t, func() bool { return src.calls.Load() >= 1 })
	time.Sleep(20 * time.Millisecond) // let the other lookups queue behind it
	close(src.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Key: %v", err)
		}
	}
	if n := src.calls.Load(); n != 1 {
		t.Errorf("50 concurrent lookups caused %d fetches, want 1", n)
	}
}

// TestCacheImpatientCaller: a caller giving up doesn't cancel the fetch for
// the others.
func TestCacheImpatientCaller(t *testing.T) {
	c, src, _ := newCache(t, CacheOptions{})
	src.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { _, err := c.Key(ctx, domain, "ob-2026-1"); done <- err }()
	patient := make(chan error)
	go func() { _, err := c.Key(context.Background(), domain, "ob-2026-1"); patient <- err }()
	waitFor(t, func() bool { return src.calls.Load() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled caller: %v", err)
	}
	close(src.gate)
	if err := <-patient; err != nil {
		t.Errorf("patient caller: %v", err)
	}
}

func (c *Cache) cached(d string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[d] != nil
}

// TestCacheEviction: when full, a newcomer may displace an entry without a
// valid profile (failed or expired), never a valid one. A newcomer that
// finds only valid entries is answered but not kept.
func TestCacheEviction(t *testing.T) {
	c, src, clk := newCache(t, CacheOptions{MaxDomains: 2, TTL: time.Hour})
	ctx := context.Background()
	src.set("a.example", "k1")
	if _, err := c.Key(ctx, "a.example", "k1"); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	_, _ = c.Key(ctx, "failed.example", "k1") // fetch fails: an entry without a profile

	src.set("b.example", "k1")
	if _, err := c.Key(ctx, "b.example", "k1"); err != nil {
		t.Fatal(err)
	}
	if !c.cached("a.example") || !c.cached("b.example") || c.cached("failed.example") {
		t.Error("the failed entry should have made room for b.example, and a.example should stay")
	}

	// Both entries are valid now: a newcomer is answered but not kept.
	src.set("c.example", "k1")
	if _, err := c.Key(ctx, "c.example", "k1"); err != nil {
		t.Fatalf("Key for a newcomer to a full cache: %v", err)
	}
	if c.cached("c.example") || !c.cached("a.example") || !c.cached("b.example") {
		t.Error("a newcomer displaced a valid entry")
	}

	// Once a's profile expires, it is the one to go.
	clk.advance(time.Hour)
	_, _ = c.Key(ctx, "b.example", "k1") // refresh b
	src.set("d.example", "k1")
	if _, err := c.Key(ctx, "d.example", "k1"); err != nil {
		t.Fatal(err)
	}
	if c.cached("a.example") || !c.cached("d.example") {
		t.Error("the expired entry wasn't the one replaced")
	}
}

// TestCacheResistsJunkDomains is the starvation scenario: envelopes naming
// junk domains whose fetches hang hold every fetch slot. A partner's valid
// entry must survive, and a pinned partner must still be able to refetch.
func TestCacheResistsJunkDomains(t *testing.T) {
	c, src, clk := newCache(t, CacheOptions{MaxDomains: 3, MaxConcurrentFetches: 2, Pinned: []string{domain}, TTL: time.Hour})
	ctx := context.Background()
	src.set("partner.example", "p1")
	for _, d := range []string{domain, "partner.example"} {
		kid := "ob-2026-1"
		if d == "partner.example" {
			kid = "p1"
		}
		if _, err := c.Key(ctx, d, kid); err != nil {
			t.Fatal(err)
		}
	}
	src.gate = make(chan struct{})
	defer close(src.gate)
	for _, junk := range []string{"junk1.example", "junk2.example"} {
		go func() { _, _ = c.Key(context.Background(), junk, "x") }()
	}
	waitFor(t, func() bool { return len(c.slots) == 2 })
	for i := range 50 {
		_, _ = c.Key(ctx, "junk-more"+string(rune('a'+i%26))+".example", "x") // refused: no slot
	}
	if !c.cached("partner.example") || !c.cached(domain) {
		t.Fatal("junk domains displaced a partner's valid entry")
	}
	if !c.IsPinned(domain) || c.IsPinned("partner.example") {
		t.Error("IsPinned disagrees with CacheOptions.Pinned")
	}
	if _, err := c.Key(ctx, "partner.example", "p1"); err != nil {
		t.Errorf("partner lookup during the flood: %v", err)
	}
	// The pinned partner's profile expires. Its refetch starts although both
	// slots are held: fetches so far are domain, partner, junk1, junk2.
	clk.advance(time.Hour)
	go func() { _, _ = c.Key(ctx, domain, "ob-2026-1") }()
	waitFor(t, func() bool { return src.calls.Load() == 5 })
	if len(c.slots) != 2 {
		t.Errorf("%d slots held, want 2: the pinned fetch must not take one", len(c.slots))
	}
}

// TestCacheReleasesSlotBeforeAnswering: by the time a lookup returns, its
// fetch no longer holds a slot, so an immediate next lookup is never
// refused as "too many fetches in flight".
func TestCacheReleasesSlotBeforeAnswering(t *testing.T) {
	c, src, _ := newCache(t, CacheOptions{MaxConcurrentFetches: 1})
	for i := range 200 {
		d := "d" + strings.Repeat("x", i%40) + string(rune('a'+i%26)) + ".example"
		src.set(d, "k1")
		if _, err := c.Key(context.Background(), d, "k1"); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
		if n := len(c.slots); n != 0 {
			t.Fatalf("lookup %d returned while its fetch still held a slot", i)
		}
	}
}

// TestCacheBoundsConcurrentFetches: envelopes naming many unknown domains
// can't start unbounded outbound fetches.
func TestCacheBoundsConcurrentFetches(t *testing.T) {
	c, src, _ := newCache(t, CacheOptions{MaxConcurrentFetches: 1})
	src.gate = make(chan struct{})
	src.set("other.example", "k1")
	done := make(chan struct{})
	go func() { _, _ = c.Key(context.Background(), domain, "ob-2026-1"); close(done) }()
	waitFor(t, func() bool { return src.calls.Load() == 1 })
	if _, err := c.Key(context.Background(), "other.example", "k1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Key beyond the fetch limit = %v, want ErrUnavailable", err)
	}
	close(src.gate)
	<-done
	// The slot is free by the time the result arrives: no waiting needed.
	if _, err := c.Key(context.Background(), "other.example", "k1"); err != nil {
		t.Errorf("Key once a slot is free: %v", err)
	}
}

// waitFor polls cond for up to 2s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached within 2s")
}
