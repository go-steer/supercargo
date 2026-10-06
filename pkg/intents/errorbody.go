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

// ulp.error codes (docs/DESIGN.md, section A.5).
const (
	CodeBadSignature      = "bad_signature"
	CodeUnknownKey        = "unknown_key"
	CodeReplay            = "replay"
	CodeStale             = "stale"
	CodeInvalidBody       = "invalid_body"
	CodeForbidden         = "forbidden"
	CodeNotFound          = "not_found"
	CodeUnsupportedIntent = "unsupported_intent"
	CodeInternal          = "internal"
)

var errorCodes = map[string]bool{
	CodeBadSignature: true, CodeUnknownKey: true, CodeReplay: true, CodeStale: true,
	CodeInvalidBody: true, CodeForbidden: true, CodeNotFound: true,
	CodeUnsupportedIntent: true, CodeInternal: true,
}

// ErrorBody is the body of ulp.error. Detail is for people, and never
// carries internal error text.
type ErrorBody struct {
	Code      string `json:"code"`
	Detail    string `json:"detail"`
	Retryable bool   `json:"retryable"`
}

// Intent implements Body.
func (*ErrorBody) Intent() string { return Error }

// Validate implements Body.
func (b *ErrorBody) Validate() error {
	var errs []error
	if !errorCodes[b.Code] {
		errs = append(errs, fmt.Errorf("code: %s is not a ulp.error code", logsafe.Quote(b.Code)))
	}
	errs = append(errs, text("detail", b.Detail, 512, false))
	return errors.Join(errs...)
}
