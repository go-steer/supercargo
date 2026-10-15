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
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-steer/supercargo/pkg/intents"
)

// Draft is a booking the assistant prepared and nobody has sent. Request
// is exactly what would be sent; RequiredBy is the user's own deadline,
// which stays on the shipper.
type Draft struct {
	ID       string
	Operator string
	Request  intents.BookingRequestBody
	// RequiredBy (YYYY-MM-DD) is private: shown in the confirmation
	// dialog as not sent, never in the request.
	RequiredBy string
	// NewPartner: a carrier not in the address book, never booked with.
	NewPartner bool
	// NewConsignee: a consignee domain not in the address book.
	NewConsignee bool
	// Disruption, if set, makes this a carrier's drafted delay instead of
	// a booking (Operator and Request are then unused).
	Disruption *Disruption
	// Order, if set, makes this a door-to-door order to a forwarder
	// instead of a booking (Operator, Request and RequiredBy are unused).
	Order *OrderDraft
	// Proposal, if set, makes this a forwarder's confirmation of a
	// delay's proposal (an amendment to a leg's carrier).
	Proposal *ProposalDraft
	Created  time.Time
}

// Drafts holds a session's drafts. Taking one removes it, so a draft is
// sent at most once.
type Drafts struct {
	// Prefix starts a booking draft's ID (default BD): an outside agent's
	// drafts are MD-, so the two never read alike.
	Prefix string
	mu     sync.Mutex
	next   int
	drafts map[string]Draft
}

// MaxDrafts bounds a session's open drafts.
const MaxDrafts = 50

// ErrNoDraft reports a draft ID that isn't open (never made, or already
// sent or discarded).
var ErrNoDraft = errors.New("assistant: no such draft")

func (d *Drafts) add(dr Draft) (Draft, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.drafts) >= MaxDrafts {
		return Draft{}, fmt.Errorf("there are already %d open drafts; send or discard one first", MaxDrafts)
	}
	if d.drafts == nil {
		d.drafts = map[string]Draft{}
	}
	d.next++
	if dr.Disruption != nil {
		dr.ID = fmt.Sprintf("DD-%04d", d.next)
		d.drafts[dr.ID] = dr
		return dr, nil
	}
	if dr.Proposal != nil {
		dr.ID = fmt.Sprintf("PD-%04d", d.next)
		d.drafts[dr.ID] = dr
		return dr, nil
	}
	if dr.Order != nil {
		dr.ID = fmt.Sprintf("OD-%04d", d.next)
		if dr.Order.Request.ShipperRef == "" {
			dr.Order.Request.ShipperRef = dr.ID
		}
		d.drafts[dr.ID] = dr
		return dr, nil
	}
	prefix := d.Prefix
	if prefix == "" {
		prefix = "BD"
	}
	dr.ID = fmt.Sprintf("%s-%04d", prefix, d.next)
	if dr.Request.ShipperRef == "" {
		dr.Request.ShipperRef = dr.ID
	}
	d.drafts[dr.ID] = dr
	return dr, nil
}

// requiredBy returns the open drafts' required-by dates.
func (d *Drafts) requiredBy() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, dr := range d.drafts {
		if dr.RequiredBy != "" {
			out = append(out, dr.RequiredBy)
		}
	}
	return out
}

// Open returns the open drafts, oldest first.
func (d *Drafts) Open() []Draft {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Draft, 0, len(d.drafts))
	for _, dr := range d.drafts {
		out = append(out, dr)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Created.Before(out[j].Created) || out[i].Created.Equal(out[j].Created) && out[i].ID < out[j].ID
	})
	return out
}

// Get returns the open draft id.
func (d *Drafts) Get(id string) (Draft, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dr, ok := d.drafts[id]
	if !ok {
		return Draft{}, ErrNoDraft
	}
	return dr, nil
}

// Take removes the open draft id and returns it: the confirmation
// dialog's step before sending. A draft that fails to send is gone too;
// the dialog may Put it back.
func (d *Drafts) Take(id string) (Draft, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dr, ok := d.drafts[id]
	if !ok {
		return Draft{}, ErrNoDraft
	}
	delete(d.drafts, id)
	return dr, nil
}

// Put returns a taken draft (one that wasn't sent after all).
func (d *Drafts) Put(dr Draft) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.drafts == nil {
		d.drafts = map[string]Draft{}
	}
	d.drafts[dr.ID] = dr
}
