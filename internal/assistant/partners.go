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

package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/supercargo/internal/directory"
	"github.com/go-steer/supercargo/internal/ports"
	"github.com/go-steer/supercargo/pkg/envelope"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Partners: the address book is the company's own list of the partners
// it knows, trusted like the rest of its configuration. A directory is
// somewhere to search for new ones; its entries are written by whoever
// listed themselves, so a found partner counts only once its profile,
// fetched from its domain, verified. Either way, the counterparty rule
// still holds: the assistant reaches a company only if the user named it,
// by its domain or by a known name (named).

// MaxVerify bounds the candidates find_partners verifies (each a profile
// fetch, at once), and VerifyTimeout how long they take together.
const (
	MaxVerify     = 5
	VerifyTimeout = 15 * time.Second
	// minNameKey is the shortest name a user's words can name a partner
	// by: "A" or "Co" names nothing.
	minNameKey = 3
)

// Candidate is a directory entry as find_partners returns it: what the
// entry claims, and whether the domain's profile verified.
type Candidate struct {
	directory.Entry
	Verified bool `json:"verified"`
	// Why the profile didn't verify, if it didn't.
	Why string `json:"why,omitempty"`
	// Known: the partner is in our address book already.
	Known bool `json:"known,omitempty"`
	// Bookable: its verified profile accepts booking requests.
	Bookable bool `json:"bookable,omitempty"`
}

// partnerEntry cleans a directory entry for a model: its text is written
// by whoever wrote the entry.
func partnerEntry(e directory.Entry) directory.Entry {
	e.Name, e.Description = OneLine(e.Name, directory.MaxName), OneLine(e.Description, directory.MaxDescription)
	e.Roles, e.Lanes = slices.Clone(e.Roles), slices.Clone(e.Lanes)
	return e
}

func (a *Assistant) listPartners(ctx context.Context, c *Conversation, input json.RawMessage) (any, error) {
	var in struct{ Role string }
	_ = json.Unmarshal(input, &in)
	var book []directory.Entry
	for _, e := range a.AddressBook {
		if in.Role == "" || slices.Contains(e.Roles, in.Role) {
			book = append(book, partnerEntry(e))
		}
	}
	out := map[string]any{"addressBook": book}
	if in.Role == "" || in.Role == directory.RoleCarrier {
		// The carriers we've booked with, from our own records.
		s, err := a.Node.Shipments(ctx)
		if err != nil {
			return nil, err
		}
		count := map[string]int{}
		for _, x := range s {
			count[x.Operator]++
		}
		type carrier struct {
			Domain   string `json:"domain"`
			Bookings int    `json:"bookings"`
		}
		var booked []carrier
		for d, n := range count {
			booked = append(booked, carrier{d, n})
		}
		sort.Slice(booked, func(i, j int) bool { return booked[i].Domain < booked[j].Domain })
		out["carriersWeBookedWith"] = booked
	}
	if found := c.foundEntries(in.Role); len(found) > 0 {
		out["foundInThisConversation"] = found
	}
	return out, nil
}

func (a *Assistant) findPartners(ctx context.Context, c *Conversation, input json.RawMessage) (any, error) {
	if a.Directory == nil {
		return nil, refuse("no partner directory is configured; the user can name a company by its domain")
	}
	var in struct{ Role, Mode, From, To, Text string }
	_ = json.Unmarshal(input, &in)
	if in.Role == "" {
		in.Role = directory.RoleCarrier
	}
	for _, p := range []string{in.From, in.To} {
		if p != "" && !directory.ValidLocode(p) {
			return nil, refuse("%q is not a UN/LOCODE", p)
		}
	}
	all, err := a.Directory.Search(ctx, directory.Query{Role: in.Role, Mode: in.Mode, From: in.From, To: in.To, Text: in.Text})
	if err != nil {
		return nil, err
	}
	// A remote directory's entries are checked like a file's: one that
	// isn't valid is dropped, and so are we.
	var entries []directory.Entry
	for _, e := range all {
		if e.Validate() == nil && e.Domain != a.Self.Domain {
			entries = append(entries, e)
		}
	}
	more := len(entries) > MaxVerify
	if more {
		entries = entries[:MaxVerify]
	}
	// A directory entry only names a domain: each is verified by fetching
	// its profile from the domain itself, all at once, within a bound.
	out := make([]Candidate, len(entries))
	vctx, cancel := context.WithTimeout(ctx, VerifyTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for i, e := range entries {
		out[i] = Candidate{Entry: partnerEntry(e), Known: a.inAddressBook(e.Domain)}
		wg.Add(1)
		go func(i int, domain string) {
			defer wg.Done()
			p, err := a.Node.Profile(vctx, domain)
			if err != nil {
				out[i].Why = OneLine(err.Error(), 200)
				return
			}
			out[i].Verified = p.Domain == domain
			if !out[i].Verified {
				out[i].Why = "the profile is for another domain"
				return
			}
			out[i].Bookable = slices.Contains(p.Intents, intents.BookingRequest)
		}(i, e.Domain)
	}
	wg.Wait()
	for _, x := range out {
		if x.Verified {
			c.addFound(x.Entry)
		}
	}
	res := map[string]any{"candidates": out,
		"note": "A directory entry is the company's own claim. Only verified candidates can be used, and only once the user names one."}
	if more {
		res["more"] = fmt.Sprintf("only the first %d matches were checked; ask the user to narrow the search", MaxVerify)
	}
	return res, nil
}

func (a *Assistant) inAddressBook(domain string) bool {
	return slices.ContainsFunc(a.AddressBook, func(e directory.Entry) bool { return e.Domain == domain })
}

// found is a verified candidate find_partners turned up. after is the
// index in userTexts from which the user's words can name it: the turns
// after the one that found it, so a request can't name what it hasn't
// seen yet.
type found struct {
	directory.Entry
	after int
}

// addFound remembers a verified candidate for this conversation.
func (c *Conversation) addFound(e directory.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.ContainsFunc(c.found, func(f found) bool { return f.Domain == e.Domain }) {
		c.found = append(c.found, found{e, len(c.userTexts)})
	}
}

func (c *Conversation) foundEntries(role string) []directory.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []directory.Entry
	for _, e := range c.found {
		if role == "" || slices.Contains(e.Roles, role) {
			out = append(out, e.Entry)
		}
	}
	return out
}

// genericWords name no company on their own: a found partner whose name
// is only these (and port names) must be named by its domain.
var genericWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an the and of for to from in on at by with our your their this that one it its
		book booking ship shipping shipment shipments carrier carriers line lines liner shipper consignee receiver
		ocean sea road rail air truck trucking freight cargo container containers logistics transport transatlantic
		express global international group company co inc ltd llc gmbh bv sa ag corp services service port ports`) {
		m[w] = true
	}
	for _, n := range ports.Names() {
		for _, w := range strings.Fields(directory.NameKey(n)) {
			m[w] = true
		}
	}
	return m
}()

// distinctive: a found partner's name the user can name it by, at least
// two words or six letters, not all of them generic.
func distinctive(key string) bool {
	words := strings.Fields(key)
	if len(words) < 2 && len([]rune(key)) < 6 {
		return false
	}
	return slices.ContainsFunc(words, func(w string) bool { return !genericWords[w] })
}

// overlaps: one name's words are a run of the other's ("Blue" and "Ocean
// Blue"), so saying the longer says the shorter too.
func overlaps(a, b string) bool {
	return a == b || strings.Contains(" "+a+" ", " "+b+" ") || strings.Contains(" "+b+" ", " "+a+" ")
}

// named is the counterparty rule: the user wrote domain in this
// conversation (Conversation.named), or wrote the name of a partner whose
// domain it is. Names are matched as whole words, case folded:
//   - a partner in our address book, by a name of at least 3 characters;
//   - a verified partner find_partners turned up, only in messages after
//     the one that found it, and only by a distinctive name.
//
// A name that overlaps another known partner's (the same, or one inside
// the other) names neither: the assistant asks. The user's words decide,
// never the model's or a tool result's.
func (a *Assistant) named(c *Conversation, domain string) error {
	if c.external && envelope.ValidDomain(domain) {
		// An outside agent's session: no user's words to check; every
		// effect still waits for a person in the node's UI (external.go).
		return nil
	}
	if err := c.named(domain); err == nil || !envelope.ValidDomain(domain) {
		return err // typed, or not even a domain
	}
	c.mu.Lock()
	texts := slices.Clone(c.userTexts)
	known := make([]found, 0, len(a.AddressBook)+len(c.found))
	for _, e := range a.AddressBook {
		known = append(known, found{e, -1})
	}
	known = append(known, c.found...)
	c.mu.Unlock()
	said := make([]string, len(texts))
	for i, t := range texts {
		said[i] = " " + directory.NameKey(t) + " "
	}
	saidName := func(key string, from int) bool {
		for _, s := range said[max(from, 0):] {
			if strings.Contains(s, " "+key+" ") {
				return true
			}
		}
		return false
	}
	for _, e := range known {
		key := directory.NameKey(e.Name)
		if e.Domain != domain || len([]rune(key)) < minNameKey || (e.after >= 0 && !distinctive(key)) || !saidName(key, e.after) {
			continue
		}
		for _, o := range known {
			if o.Domain != domain && overlaps(key, directory.NameKey(o.Name)) {
				return refuse("%q could mean %s or %s (%q); ask the user which domain they mean",
					OneLine(e.Name, 64), domain, o.Domain, OneLine(o.Name, 64))
			}
		}
		return nil
	}
	return errTool{errNotNamed.Error()}
}
