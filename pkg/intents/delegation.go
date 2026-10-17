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

package intents

import (
	"errors"
	"fmt"

	"github.com/go-steer/supercargo/pkg/internal/logsafe"
)

// Delegated access (docs/DESIGN.md, A.5 "Delegated access"; milestone 7):
// the booking's shipper hands a grantee a delegation token
// (ulp.delegation.grant, the token in attachments.delegation); the grantee
// presents it to the carrier to subscribe to the booking's events and
// alerts (ulp.tnt.subscription.request), or with an inquiry or status
// request. Every body is ULP's own (no dialect).

// Delegated access intents.
const (
	TnTSubscriptionRequest      = "ulp.tnt.subscription.request"
	TnTSubscriptionConfirmation = "ulp.tnt.subscription.confirmation"
)

// MaxNote bounds a grant's note: the issuer's own words to the grantee.
const MaxNote = 280

// DelegationGrantBody is a grant handed to its grantee: the token itself
// travels in attachments.delegation, signed by its issuer; Note is the
// issuer's message to the grantee.
type DelegationGrantBody struct {
	Note string `json:"note,omitempty"`
}

// TnTSubscriptionRequestBody asks the carrier to push the booking's
// events and alerts to Subscription, on the grantee's own domain.
type TnTSubscriptionRequestBody struct {
	Subscription Subscription `json:"subscription"`
}

// TnTSubscriptionConfirmationBody is the carrier's answer: confirmed,
// with the subscription's ID, its end (the grant's), and the leg as the
// carrier plans it (its places and times, no more); or rejected, with
// why.
type TnTSubscriptionConfirmationBody struct {
	Status         string      `json:"status"`
	SubscriptionID string      `json:"subscription_id,omitempty"`
	ExpiresAt      string      `json:"expires_at,omitempty"`
	Leg            *PlannedLeg `json:"leg,omitempty"`
	Messages       []Message   `json:"messages,omitempty"`
}

// Intent implements Body.
func (*DelegationGrantBody) Intent() string { return DelegationGrant }

// Intent implements Body.
func (*TnTSubscriptionRequestBody) Intent() string { return TnTSubscriptionRequest }

// Intent implements Body.
func (*TnTSubscriptionConfirmationBody) Intent() string { return TnTSubscriptionConfirmation }

// Validate implements Body.
func (b *DelegationGrantBody) Validate() error {
	if b.Note != "" {
		return text("note", b.Note, MaxNote, false)
	}
	return nil
}

// Validate implements Body.
func (b *TnTSubscriptionRequestBody) Validate() error {
	return b.Subscription.validate()
}

// Validate implements Body.
func (b *TnTSubscriptionConfirmationBody) Validate() error {
	errs := []error{validateMessages(b.Messages)}
	switch b.Status {
	case StatusConfirmed:
		errs = append(errs, ref("subscription_id", b.SubscriptionID))
		if _, err := timestamp("expires_at", b.ExpiresAt); err != nil {
			errs = append(errs, err)
		}
		if b.Leg == nil {
			errs = append(errs, errors.New("leg: a confirmation names the leg"))
		} else {
			errs = append(errs, b.Leg.validate("leg"))
		}
	case StatusRejected:
		if len(b.Messages) == 0 {
			errs = append(errs, errors.New("a rejection has no messages"))
		}
		if b.SubscriptionID != "" || b.ExpiresAt != "" || b.Leg != nil {
			errs = append(errs, errors.New("a rejection carries no subscription"))
		}
	default:
		errs = append(errs, fmt.Errorf("status: %s is not confirmed or rejected", logsafe.Quote(b.Status)))
	}
	return errors.Join(errs...)
}
