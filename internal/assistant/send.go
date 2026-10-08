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
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/supercargo/internal/shipper"
	"github.com/go-steer/supercargo/pkg/intents"
)

// Sent is a draft that was sent, and the carrier's answer.
type Sent struct {
	Draft        Draft
	Confirmation *intents.BookingConfirmationBody
}

// ErrOutcomeUnknown reports a send that may or may not have reached the
// carrier (the node didn't answer, or the carrier didn't): the draft is
// gone, so a second yes can't book twice. list_shipments tells.
var ErrOutcomeUnknown = errors.New("assistant: the booking may or may not have been sent; check the shipments before trying again")

// Send sends the open draft id through the node: the confirmation
// dialog's action, after a person said yes. Nothing in the tool loop
// calls it. The draft is taken first, so it is sent at most once; it is
// put back only when the node refused it before sending (an HTTP 4xx).
func (a *Assistant) Send(ctx context.Context, id string) (sent *Sent, err error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "ulp.booking.send", trace.WithAttributes(attribute.String("ulp.draft_id", OneLine(id, 32))))
	span.AddEvent("confirmed in the dialog")
	defer func() {
		switch {
		case errors.Is(err, ErrOutcomeUnknown):
			span.SetStatus(codes.Error, "outcome_unknown")
		case err != nil:
			span.SetStatus(codes.Error, "not_sent")
		default:
			span.SetAttributes(attribute.String("ulp.booking_status", sent.Confirmation.Status))
		}
		span.End()
	}()
	d, err := a.Drafts.Take(id)
	if err != nil {
		return nil, err
	}
	resp, err := a.Node.Book(ctx, shipper.BookRequest{Operator: d.Operator, Request: d.Request})
	if err != nil {
		var ne *NodeError
		var op *net.OpError
		switch {
		case errors.As(err, &ne) && ne.Status >= http.StatusBadRequest && ne.Status < http.StatusInternalServerError:
			// Refused before anything was sealed.
			a.Drafts.Put(d)
			if strings.HasPrefix(err.Error(), "not sent") {
				return nil, err
			}
			return nil, fmt.Errorf("not sent: %w", err)
		case errors.As(err, &op) && op.Op == "dial":
			// The node isn't running: nothing left the shipper.
			a.Drafts.Put(d)
			return nil, fmt.Errorf("not sent: is the node running? %w", err)
		}
		return nil, fmt.Errorf("%w (%v)", ErrOutcomeUnknown, err)
	}
	if resp.Confirmation == nil {
		return nil, fmt.Errorf("%w (the node answered without a confirmation)", ErrOutcomeUnknown)
	}
	return &Sent{Draft: d, Confirmation: resp.Confirmation}, nil
}

// notePrefix marks a message the UI wrote into the conversation: it is
// not the user's, so it never counts as the user naming a company.
const notePrefix = "[ulpchat] "

// Report tells the assistant what came of a send, as a turn of its own,
// so it can tell the user (comparing the planned arrival with their
// deadline, say). The note is the code's, not the user's words.
func (a *Assistant) Report(ctx context.Context, c *Conversation, s *Sent) (Reply, error) {
	result, err := json.Marshal(map[string]any{"draftId": s.Draft.ID, "operator": s.Draft.Operator,
		"confirmation": s.Confirmation, "requiredBy": s.Draft.RequiredBy})
	if err != nil {
		return Reply{}, err
	}
	note := notePrefix + "The user confirmed draft " + s.Draft.ID + " in the dialog, and our node sent it, signed. " +
		"The carrier's answer is below; tell the user the outcome in a sentence or two, and compare the planned arrival with their deadline if they gave one.\n" +
		"<result>\n" + string(result) + "\n</result>"
	r, err := a.turn(ctx, c, note, false)
	if err != nil {
		// The send happened: the conversation keeps the fact even when
		// the model can't be told now.
		_ = c.Note(strings.TrimPrefix(note, notePrefix))
	}
	return r, err
}
