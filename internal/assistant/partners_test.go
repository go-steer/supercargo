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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/supercargo/internal/directory"
	"github.com/go-steer/supercargo/internal/llm"
	"github.com/go-steer/supercargo/internal/llm/fake"
	"github.com/go-steer/supercargo/internal/store"
)

var (
	oceanBlue = directory.Entry{Domain: "ocean-blue.example", Name: "Ocean Blue", Roles: []string{directory.RoleCarrier}}
	footwear  = directory.Entry{Domain: "cymbal-footwear.example", Name: "Cymbal Footwear", Roles: []string{directory.RoleConsignee}}
)

// oneCall runs a turn whose model makes call, then answers; it returns
// the tool event.
func oneCall(t *testing.T, a *Assistant, tools *[]ToolEvent, c *Conversation, text string, call llm.ToolCall) ToolEvent {
	t.Helper()
	a.Model = fake.Turns(llm.GenerateResponse{ToolCalls: []llm.ToolCall{call}}, llm.GenerateResponse{Text: "OK."})
	*tools = nil
	if _, err := a.Turn(context.Background(), c, text); err != nil {
		t.Fatal(err)
	}
	if len(*tools) != 1 {
		t.Fatalf("%q: tools %+v", text, *tools)
	}
	return (*tools)[0]
}

// TestNamedByName: a partner in the address book can be named by its
// name, as whole words; part of a name, a too-short name, another
// partner's name, or a name two partners share names nobody.
func TestNamedByName(t *testing.T) {
	profile := func(d string) llm.ToolCall { return fake.Call("1", ToolGetProfile, `{"domain":"`+d+`"}`) }
	for text, want := range map[string]string{
		"What does Ocean Blue offer?":        "",
		"what does OCEAN-BLUE offer":         "",
		"Ocean Blue, Inc. — what do they do": "",
		"What does Blue offer?":              "hasn't named",
		"What does Oceanblue offer?":         "hasn't named",
		"What does Cymbal Footwear offer?":   "hasn't named", // a partner, but not this domain's
	} {
		a, tools, _ := newAssistant(nil, &node{})
		a.AddressBook = []directory.Entry{oceanBlue, footwear}
		ev := oneCall(t, a, tools, &Conversation{}, text, profile("ocean-blue.example"))
		if (want == "") != (ev.Error == "") || !strings.Contains(ev.Error, want) {
			t.Errorf("%q: error %q, want %q", text, ev.Error, want)
		}
	}
	// Two partners called Ocean Blue: the name names neither.
	a, tools, _ := newAssistant(nil, &node{})
	a.AddressBook = []directory.Entry{oceanBlue, {Domain: "ocean-blue-lines.example", Name: "ocean blue", Roles: []string{directory.RoleCarrier}}}
	if ev := oneCall(t, a, tools, &Conversation{}, "Ask Ocean Blue", profile("ocean-blue.example")); !strings.Contains(ev.Error, "two partners") {
		t.Errorf("an ambiguous name: %q", ev.Error)
	}
	// The domain itself still works.
	if ev := oneCall(t, a, tools, &Conversation{}, "Ask ocean-blue.example", profile("ocean-blue.example")); ev.Error != "" {
		t.Errorf("the domain: %q", ev.Error)
	}
	// Too short to name anyone.
	a.AddressBook = []directory.Entry{{Domain: "ocean-blue.example", Name: "OB", Roles: []string{directory.RoleCarrier}}}
	if ev := oneCall(t, a, tools, &Conversation{}, "Ask OB", profile("ocean-blue.example")); ev.Error == "" {
		t.Error("a two-letter name named a partner")
	}
	// The consignee, by name, in a draft.
	a.AddressBook = []directory.Entry{oceanBlue, footwear}
	draft := fake.Call("1", ToolDraftBooking, draftJSON(`"consigneeName":"Cymbal Footwear","consigneeDomain":"cymbal-footwear.example"`))
	if ev := oneCall(t, a, tools, &Conversation{}, "Book with Ocean Blue to Cymbal Footwear, Rotterdam to Newark", draft); ev.Error != "" || ev.Draft == nil {
		t.Errorf("a draft by names: %+v", ev)
	}
	// Not a domain at all: refused as such, whatever the names.
	if ev := oneCall(t, a, tools, &Conversation{}, "Ocean Blue", profile("Ocean Blue")); !strings.Contains(ev.Error, "not a domain") {
		t.Errorf("not a domain: %q", ev.Error)
	}
}

// directoryOf is a directory with these entries.
func directoryOf(t *testing.T, es ...directory.Entry) directory.Directory {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"entries": es}) // JSON is YAML
	f, err := directory.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// lastResult is the latest tool result the model saw.
func lastResult(a *Assistant) string {
	msgs := a.Model.(*fake.Model).Calls()[1].Messages
	return msgs[len(msgs)-1].ToolResults[0].Content
}

// rawDirectory returns its entries unchecked, as a remote directory might.
type rawDirectory []directory.Entry

func (r rawDirectory) Search(context.Context, directory.Query) ([]directory.Entry, error) {
	return r, nil
}

type brokenDirectory struct{}

func (brokenDirectory) Search(context.Context, directory.Query) ([]directory.Entry, error) {
	return nil, errors.New("the directory is down")
}

// TestFindPartners: candidates come back with their profiles verified;
// a verified one can then be named by its name, an unverified one can't,
// and we never find ourselves.
func TestFindPartners(t *testing.T) {
	n := &node{}
	a, tools, _ := newAssistant(nil, n)
	northStar := directory.Entry{Domain: "north-star.example", Name: "North Star Lines", Roles: []string{directory.RoleCarrier},
		Description: "Weekly\u202e service"}
	us := directory.Entry{Domain: "cymbal-shoes.example", Name: "Cymbal Shoes", Roles: []string{directory.RoleCarrier}}
	a.Directory = rawDirectory{oceanBlue, northStar, us}
	a.AddressBook = []directory.Entry{footwear}
	var c Conversation
	ev := oneCall(t, a, tools, &c, "Find me a carrier from Rotterdam to Newark", fake.Call("1", ToolFind, `{"from":"NLRTM","to":"USEWR"}`))
	if ev.Error != "" {
		t.Fatal(ev.Error)
	}
	res := lastResult(a)
	var got struct{ Candidates []Candidate }
	_ = json.Unmarshal([]byte(res), &got)
	if len(got.Candidates) != 2 || !got.Candidates[0].Verified || !got.Candidates[0].Bookable ||
		got.Candidates[1].Verified || got.Candidates[1].Why == "" || strings.ContainsRune(got.Candidates[1].Description, '\u202e') {
		t.Fatalf("candidates %+v", got.Candidates)
	}
	if ev := oneCall(t, a, tools, &c, "Book with Ocean Blue", fake.Call("1", ToolGetProfile, `{"domain":"ocean-blue.example"}`)); ev.Error != "" {
		t.Errorf("a verified candidate by name: %q", ev.Error)
	}
	if ev := oneCall(t, a, tools, &c, "Book with North Star Lines", fake.Call("1", ToolGetProfile, `{"domain":"north-star.example"}`)); ev.Error == "" {
		t.Error("an unverified candidate was usable by name")
	}
	// Another conversation hasn't found anyone.
	if ev := oneCall(t, a, tools, &Conversation{}, "Book with Ocean Blue", fake.Call("1", ToolGetProfile, `{"domain":"ocean-blue.example"}`)); ev.Error == "" {
		t.Error("a candidate found in one conversation was named in another")
	}
	// Refusals.
	for name, call := range map[string]llm.ToolCall{
		"a bad port":       fake.Call("1", ToolFind, `{"from":"Rotterdam"}`),
		"a lowercase port": fake.Call("1", ToolFind, `{"to":"usewr"}`),
		"a digit too soon": fake.Call("1", ToolFind, `{"to":"U2EWR"}`),
		"a bad mode":       fake.Call("1", ToolFind, `{"mode":"SEA"}`),
	} {
		if ev := oneCall(t, a, tools, &Conversation{}, "find", call); ev.Error == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	// A profile that answers for another domain doesn't verify this one.
	a.Directory = rawDirectory{{Domain: "alias.example", Name: "Alias", Roles: []string{directory.RoleCarrier}}}
	oneCall(t, a, tools, &Conversation{}, "find", fake.Call("1", ToolFind, `{}`))
	if res := lastResult(a); !strings.Contains(res, `"verified":false`) || !strings.Contains(res, "another domain") {
		t.Errorf("a profile for another domain: %s", res)
	}
	a.Directory = brokenDirectory{}
	if ev := oneCall(t, a, tools, &Conversation{}, "find", fake.Call("1", ToolFind, `{}`)); !strings.Contains(ev.Error, "down") {
		t.Errorf("a broken directory: %q", ev.Error)
	}
	a.Directory = nil
	if ev := oneCall(t, a, tools, &Conversation{}, "find", fake.Call("1", ToolFind, `{}`)); !strings.Contains(ev.Error, "no partner directory") {
		t.Errorf("no directory: %q", ev.Error)
	}
}

// TestFindPartnersCap: at most MaxVerify candidates are checked.
func TestFindPartnersCap(t *testing.T) {
	var es []directory.Entry
	for _, d := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		es = append(es, directory.Entry{Domain: d + "-lines.example", Name: d, Roles: []string{directory.RoleCarrier}})
	}
	n := &node{}
	a, tools, _ := newAssistant(nil, n)
	a.Directory = directoryOf(t, es...)
	oneCall(t, a, tools, &Conversation{}, "find", fake.Call("1", ToolFind, `{}`))
	res := lastResult(a)
	var got struct {
		Candidates []Candidate
		More       string
	}
	_ = json.Unmarshal([]byte(res), &got)
	if len(got.Candidates) != MaxVerify || got.More == "" {
		t.Errorf("%d candidates, more %q", len(got.Candidates), got.More)
	}
}

// TestListPartners: the address book, the carriers we booked with, and
// what this conversation found, by role.
func TestListPartners(t *testing.T) {
	n := &node{shipments: []store.Summary{{Ref: "OB-1", Operator: "ocean-blue.example"}, {Ref: "OB-2", Operator: "ocean-blue.example"},
		{Ref: "NS-1", Operator: "north-star.example"}}}
	a, tools, _ := newAssistant(nil, n)
	a.AddressBook = []directory.Entry{oceanBlue, footwear}
	var c Conversation
	c.addFound(directory.Entry{Domain: "anywhere.example", Name: "Anywhere", Roles: []string{directory.RoleCarrier}})
	oneCall(t, a, tools, &c, "who do we know?", fake.Call("1", ToolPartners, `{}`))
	res := lastResult(a)
	for _, want := range []string{`"cymbal-footwear.example"`, `{"domain":"ocean-blue.example","bookings":2}`, `{"domain":"north-star.example","bookings":1}`, `"anywhere.example"`} {
		if !strings.Contains(res, want) {
			t.Errorf("list_partners lacks %s: %s", want, res)
		}
	}
	oneCall(t, a, tools, &c, "our consignees?", fake.Call("1", ToolPartners, `{"role":"consignee"}`))
	res = lastResult(a)
	if !strings.Contains(res, "cymbal-footwear.example") || strings.Contains(res, "ocean-blue.example") || strings.Contains(res, "anywhere.example") {
		t.Errorf("consignees only: %s", res)
	}
}

// TestFakePartnerScenes: the fake model's partner scenes. Who do we know,
// find a carrier, book with it and our consignee by their names; and in a
// new conversation, a carrier from the address book by name.
func TestFakePartnerScenes(t *testing.T) {
	n := &node{}
	a, tools, _ := newAssistant(FakeModel(func() time.Time { return thursday }), n)
	northStar := directory.Entry{Domain: "north-star.example", Name: "North Star Lines", Roles: []string{directory.RoleCarrier}}
	a.Directory = rawDirectory{oceanBlue, northStar}
	a.AddressBook = []directory.Entry{footwear}
	var c Conversation
	turn := func(c *Conversation, text string) Reply {
		t.Helper()
		*tools = nil
		r, err := a.Turn(context.Background(), c, text)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := turn(&c, "Who do we know?"); !strings.Contains(r.Text, "**Cymbal Footwear** (`cymbal-footwear.example`)") {
		t.Errorf("who do we know: %q", r.Text)
	}
	r := turn(&c, "Find me a carrier from Rotterdam to Newark")
	if !strings.Contains(r.Text, "**Ocean Blue** (`ocean-blue.example`): verified, takes bookings") || !strings.Contains(r.Text, "didn't verify") {
		t.Errorf("find: %q", r.Text)
	}
	book := "Book one 40ft high-cube of sneakers for CY-PO-55120, Rotterdam to Newark, departing 2026-10-06, with Ocean Blue. " +
		"Consignee is Cymbal Footwear. It has to arrive before Thursday the 15th."
	r = turn(&c, book)
	if len(r.Drafts) != 1 || r.Drafts[0].To != "ocean-blue.example" || r.Drafts[0].Request.Consignee == nil ||
		r.Drafts[0].Request.Consignee.Domain != "cymbal-footwear.example" || !r.Drafts[0].NewPartner {
		t.Fatalf("book by names: %+v, tools %+v", r, *tools)
	}
	// An unverified candidate, by name: no carrier, so no draft.
	if r := turn(&c, strings.Replace(book, "Ocean Blue", "North Star Lines", 1)); len(r.Drafts) != 0 {
		t.Errorf("a draft with an unverified carrier: %+v", r.Drafts)
	}
	// A new conversation: Ocean Blue is in the address book now, and the
	// assistant looks it up by name before drafting.
	a.AddressBook = []directory.Entry{footwear, oceanBlue}
	r = turn(&Conversation{}, book)
	if len(r.Drafts) != 1 || r.Drafts[0].To != "ocean-blue.example" || (*tools)[0].Name != ToolPartners || r.Drafts[0].NewPartner {
		t.Errorf("book from the address book: %+v, tools %+v", r, *tools)
	}
	// Nobody to list.
	a.AddressBook = nil
	if r := turn(&Conversation{}, "Who are our partners?"); !strings.Contains(r.Text, "don't know any partners") {
		t.Errorf("no partners: %q", r.Text)
	}
	a.Directory = rawDirectory{}
	if r := turn(&Conversation{}, "find a carrier"); !strings.Contains(r.Text, "no carrier") {
		t.Errorf("an empty directory: %q", r.Text)
	}
}

// TestNewPartner: a draft to a carrier we neither list nor ever booked
// with says so.
func TestNewPartner(t *testing.T) {
	draft := fake.Call("1", ToolDraftBooking, draftJSON(""))
	for name, c := range map[string]struct {
		book      []directory.Entry
		shipments []store.Summary
		want      bool
	}{
		"new":                {nil, nil, true},
		"in the book":        {[]directory.Entry{oceanBlue}, nil, false},
		"booked with before": {nil, []store.Summary{{Ref: "OB-1", Operator: "ocean-blue.example"}}, false},
	} {
		a, tools, _ := newAssistant(nil, &node{shipments: c.shipments})
		a.AddressBook = c.book
		ev := oneCall(t, a, tools, &Conversation{}, "Book with ocean-blue.example", draft)
		if ev.Draft == nil || ev.Draft.NewPartner != c.want {
			t.Errorf("%s: %+v", name, ev)
		}
	}
}
