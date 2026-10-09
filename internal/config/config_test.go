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

package config

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const operatorYAML = `
domain: ocean-blue.example
name: Ocean Blue
role: operator
listen: ":8443"
tls: { mode: local-ca, certFile: /certs/ob.pem, keyFile: /certs/ob-key.pem, caFile: /certs/ca.pem }
keys:
  - kid: ob-2026-1
    privateKeyFile: /keys/ob-2026-1.pem
peers:
  cymbal-shoes.example: cymbal-shoes:8443
partners: [cymbal-shoes.example, cymbal-footwear.example]
discovery: { ttl: 1h, refreshFloor: 1m }
security: { maxSkew: 5m, replayRetention: 15m }
operator:
  refPrefix: OB-BKG
  containerPrefix: OCBU
  vessel: Ocean Blue Aurora
  voyage: 42W
  departureHour: 18
  transit: 134h
  deny: [mallory.example]
llm: { provider: fake, model: m, apiKeyEnv: LLM_API_KEY }
skills: { dir: /etc/ulp/skills }
mcp: { connectors: [{ name: tms, url: "https://tms.internal.ocean-blue.example/mcp" }] }
simulator: { hoursPerSecond: 1 }
control: { socket: /run/ulp/ocean-blue.sock }
telemetry: { endpoint: "localhost:4318", insecure: true, sampleRatio: 0.5, serviceName: ocean-blue }
`

func TestParse(t *testing.T) {
	c, err := Parse([]byte(operatorYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Domain != "ocean-blue.example" || c.Operator.Transit != 134*time.Hour || c.Security.ReplayRetention != 15*time.Minute ||
		c.Peers["cymbal-shoes.example"] != "cymbal-shoes:8443" || c.Discovery.TTL != time.Hour {
		t.Errorf("parsed %+v", c)
	}
	if tm := c.Telemetry; tm == nil || tm.Protocol != "http" || tm.Endpoint != "localhost:4318" || !tm.Insecure || *tm.SampleRatio != 0.5 {
		t.Errorf("telemetry %+v", c.Telemetry)
	}
	pinned := c.Pinned()
	slices.Sort(pinned)
	if !slices.Equal(pinned, []string{"cymbal-footwear.example", "cymbal-shoes.example"}) {
		t.Errorf("Pinned = %v", pinned)
	}
	// Defaults: skew 5m, retention 3× skew.
	minimal := `{domain: cymbal-shoes.example, name: Cymbal, role: shipper, listen: ":8443", tls: {mode: insecure-dev},
keys: [{kid: cy-1, privateKeyFile: k.pem}]}`
	c, err = Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse(minimal): %v", err)
	}
	if c.Security.MaxSkew != 5*time.Minute || c.Security.ReplayRetention != 15*time.Minute {
		t.Errorf("defaults: %+v", c.Security)
	}
	if c.Simulator != nil || c.Control != nil {
		t.Error("a shipper got simulator or control settings")
	}
	// A shipper has no simulator, but may have a control socket.
	if _, err := Parse([]byte(strings.TrimSuffix(minimal, "}") + ", simulator: {hoursPerSecond: 1}}")); err == nil {
		t.Error("a shipper with a simulator was accepted")
	}
	c, err = Parse([]byte(strings.TrimSuffix(minimal, "}") + ", control: {socket: /tmp/cy/ulp.sock}}"))
	if err != nil || c.Control.Socket != "/tmp/cy/ulp.sock" {
		t.Errorf("a shipper's control socket: %+v, %v", c, err)
	}
	// Business context: a shipper's, at an absolute path.
	c, err = Parse([]byte(strings.TrimSuffix(minimal, "}") + ", business: {file: /etc/ulp/business.yaml}}"))
	if err != nil || c.Business.File != "/etc/ulp/business.yaml" {
		t.Errorf("a shipper's business file: %+v, %v", c, err)
	}
	for _, bad := range []string{", business: {file: business.yaml}}", ", business: {file: ''}}"} {
		if _, err := Parse([]byte(strings.TrimSuffix(minimal, "}") + bad)); err == nil {
			t.Errorf("a shipper with %s was accepted", bad)
		}
	}
	if _, err := Parse([]byte(operatorYAML + "business: { file: /etc/ulp/business.yaml }\n")); err == nil {
		t.Error("an operator with a business file was accepted")
	}
	// The MCP endpoint: the shipper's, with its token from one place.
	c, err = Parse([]byte(strings.TrimSuffix(minimal, "}") + `, mcp: {server: {tokenFile: /etc/ulp/mcp-token, uiURL: "https://cymbal-shoes.example/ui/"}}}`))
	if err != nil || c.MCP.Server.TokenFile != "/etc/ulp/mcp-token" {
		t.Errorf("mcp server: %+v, %v", c, err)
	}
	for _, bad := range []string{
		", mcp: {server: {}}}",
		", mcp: {server: {tokenEnv: T, tokenFile: /x}}}",
		", mcp: {server: {tokenEnv: 'not a name'}}}",
		", mcp: {server: {tokenFile: relative/token}}}",
		", mcp: {server: {tokenEnv: T, uiURL: 'ftp://x'}}}",
	} {
		if _, err := Parse([]byte(strings.TrimSuffix(minimal, "}") + bad)); err == nil {
			t.Errorf("a config with %s was accepted", bad)
		}
	}
	if _, err := Parse([]byte(operatorYAML + "mcp: { server: { tokenEnv: T } }\n")); err == nil {
		t.Error("an operator-only node with the shipper's MCP tools was accepted")
	}
	// The address book and the directory: files at absolute paths.
	c, err = Parse([]byte(strings.TrimSuffix(minimal, "}") + ", addressBook: {file: /etc/ulp/partners.yaml}, directory: {file: /etc/ulp/directory.yaml}}"))
	if err != nil || c.AddressBook.File != "/etc/ulp/partners.yaml" || c.Directory.File != "/etc/ulp/directory.yaml" {
		t.Errorf("address book and directory: %+v, %v", c, err)
	}
	for _, bad := range []string{", addressBook: {file: partners.yaml}}", ", directory: {file: ''}}", ", directory: {url: x}}"} {
		if _, err := Parse([]byte(strings.TrimSuffix(minimal, "}") + bad)); err == nil {
			t.Errorf("a config with %s was accepted", bad)
		}
	}
	// Anthropic: the key's variable defaults to ANTHROPIC_API_KEY.
	c, err = Parse([]byte(strings.Replace(operatorYAML, "provider: fake, model: m, apiKeyEnv: LLM_API_KEY", "provider: anthropic", 1)))
	if err != nil || c.LLM.APIKeyEnv != "ANTHROPIC_API_KEY" || c.LLM.Model != "" {
		t.Errorf("anthropic defaults: %+v, %v", c.LLM, err)
	}
	// An operator's clock defaults to 1h/s.
	c, err = Parse([]byte(strings.Replace(operatorYAML, "simulator: { hoursPerSecond: 1 }\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Simulator.HoursPerSecond != 1 || c.Control.Socket != "/run/ulp/ocean-blue.sock" {
		t.Errorf("operator defaults: %+v %+v", c.Simulator, c.Control)
	}
}

func TestParseRejects(t *testing.T) {
	for name, edit := range map[string][2]string{
		"unknown key":                 {"name: Ocean Blue", "name: Ocean Blue\nfrobnicate: yes"},
		"misspelled security key":     {"maxSkew: 5m", "maxskew: 5m"},
		"bad domain":                  {"domain: ocean-blue.example", "domain: Ocean Blue"},
		"no name":                     {"name: Ocean Blue", "name: \"\""},
		"bad role":                    {"role: operator", "role: admin"},
		"role and roles":              {"role: operator", "role: operator\nroles: [operator]"},
		"no roles":                    {"role: operator", "roles: []"},
		"unknown role in roles":       {"role: operator", "roles: [operator, admin]"},
		"duplicate role":              {"role: operator", "roles: [operator, operator]"},
		"operator section, no role":   {"role: operator", "roles: [shipper, receiver]"},
		"bad listen":                  {`listen: ":8443"`, "listen: 8443"},
		"bad tls mode":                {"mode: local-ca", "mode: none"},
		"local-ca without cert":       {"certFile: /certs/ob.pem, ", ""},
		"bad kid":                     {"kid: ob-2026-1", "kid: ob 1"},
		"no key file":                 {"privateKeyFile: /keys/ob-2026-1.pem", "privateKeyFile: \"\""},
		"two keys":                    {"    privateKeyFile: /keys/ob-2026-1.pem", "    privateKeyFile: /keys/ob-2026-1.pem\n  - kid: ob-2\n    privateKeyFile: /k2"},
		"peer key not a domain":       {"cymbal-shoes.example: cymbal-shoes:8443", "10.0.0.1: cymbal-shoes:8443"},
		"peer target without port":    {"cymbal-shoes.example: cymbal-shoes:8443", "cymbal-shoes.example: cymbal-shoes"},
		"partner not a domain":        {"partners: [cymbal-shoes.example", "partners: [Cymbal"},
		"negative ttl":                {"ttl: 1h", "ttl: -1h"},
		"negative skew":               {"maxSkew: 5m", "maxSkew: -5m"},
		"retention not over 2× skew":  {"replayRetention: 15m", "replayRetention: 10m"},
		"bad duration":                {"ttl: 1h", "ttl: an hour"},
		"short container prefix":      {"containerPrefix: OCBU", "containerPrefix: OCB"},
		"lowercase container prefix":  {"containerPrefix: OCBU", "containerPrefix: ocbu"},
		"bad departure hour":          {"departureHour: 18", "departureHour: 24"},
		"no transit":                  {"transit: 134h", "transit: 0s"},
		"deny entry not a domain":     {"deny: [mallory.example]", "deny: [\"*\"]"},
		"negative clock":              {"hoursPerSecond: 1", "hoursPerSecond: -1"},
		"clock over the cap":          {"hoursPerSecond: 1", "hoursPerSecond: 3601"},
		"NaN clock":                   {"hoursPerSecond: 1", "hoursPerSecond: .nan"},
		"unknown llm provider":        {"provider: fake", "provider: oracle"},
		"api key env not a name":      {"provider: fake, model: m, apiKeyEnv: LLM_API_KEY", "provider: anthropic, model: m, apiKeyEnv: \"sk-ant-123 key\""},
		"model not an id":             {"provider: fake, model: m,", "provider: anthropic, model: \"m; rm -rf\","},
		"relative skills dir":         {"dir: /etc/ulp/skills", "dir: skills"},
		"relative control socket":     {"socket: /run/ulp/ocean-blue.sock", "socket: ulp.sock"},
		"unclean control socket":      {"socket: /run/ulp/ocean-blue.sock", "socket: /run/ulp/../ulp.sock"},
		"long control socket":         {"socket: /run/ulp/ocean-blue.sock", "socket: /" + strings.Repeat("x", 100)},
		"empty control socket":        {"socket: /run/ulp/ocean-blue.sock", `socket: ""`},
		"telemetry ratio over 1":      {"sampleRatio: 0.5", "sampleRatio: 1.5"},
		"telemetry ratio NaN":         {"sampleRatio: 0.5", "sampleRatio: .nan"},
		"telemetry bad endpoint":      {`endpoint: "localhost:4318"`, `endpoint: "localhost"`},
		"telemetry URL over grpc":     {`endpoint: "localhost:4318"`, `protocol: grpc, endpoint: "http://localhost:4317"`},
		"telemetry ftp URL":           {`endpoint: "localhost:4318"`, `endpoint: "ftp://localhost:4318"`},
		"telemetry http, secure":      {`endpoint: "localhost:4318", insecure: true`, `endpoint: "http://localhost:4318"`},
		"telemetry https, insecure":   {`endpoint: "localhost:4318"`, `endpoint: "https://collector.example"`},
		"telemetry URL credentials":   {`endpoint: "localhost:4318"`, `endpoint: "http://u:p@localhost:4318"`},
		"telemetry bad metrics URL":   {"serviceName: ocean-blue", "serviceName: ocean-blue, metricsEndpoint: \"ftp://p:9090\""},
		"telemetry metrics creds":     {"serviceName: ocean-blue", "serviceName: ocean-blue, metricsEndpoint: \"http://u:p@p:9090\""},
		"telemetry metrics https":     {"serviceName: ocean-blue", "serviceName: ocean-blue, metricsEndpoint: \"https://p:9090\""},
		"telemetry negative interval": {"serviceName: ocean-blue", "serviceName: ocean-blue, metricInterval: -1s"},
		"telemetry bad trust peer":    {"serviceName: ocean-blue", "serviceName: ocean-blue, trustPeers: [\"*\"]"},
		"telemetry bad protocol":      {`endpoint: "localhost:4318"`, `protocol: udp, endpoint: "localhost:4318"`},
		"telemetry bad service name":  {"serviceName: ocean-blue", "serviceName: \"ocean\\u202eblue\""},
	} {
		t.Run(name, func(t *testing.T) {
			in := strings.Replace(operatorYAML, edit[0], edit[1], 1)
			if in == operatorYAML {
				t.Fatalf("edit %q didn't apply", edit[0])
			}
			if _, err := Parse([]byte(in)); err == nil {
				t.Error("Parse succeeded")
			}
		})
	}
	// An operator with no operator section at all: only the section check
	// can refuse it (an unknown key would be caught by strict decoding).
	section := "operator:\n  refPrefix: OB-BKG\n  containerPrefix: OCBU\n  vessel: Ocean Blue Aurora\n  voyage: 42W\n  departureHour: 18\n  transit: 134h\n  deny: [mallory.example]\n"
	if !strings.Contains(operatorYAML, section) {
		t.Fatal("fixture changed: operator section not found")
	}
	if _, err := Parse([]byte(strings.Replace(operatorYAML, section, "", 1))); err == nil {
		t.Error("an operator without an operator section was accepted")
	}
	if _, err := Parse([]byte(strings.Replace(operatorYAML, section, "operator: ~\n", 1))); err == nil {
		t.Error("an operator with a null operator section was accepted")
	}
	// A second YAML document would otherwise be silently ignored.
	if _, err := Parse([]byte(operatorYAML + "---\nsecurity: { maxSkew: 1m }\n")); err == nil {
		t.Error("a second YAML document was accepted")
	}
	for name, edit := range map[string][2]string{
		"skew over an hour":           {"security: { maxSkew: 5m, replayRetention: 15m }", "security: { maxSkew: 100000h, replayRetention: 300001h }"},
		"retention over a day":        {"replayRetention: 15m", "replayRetention: 25h"},
		"ttl over a day":              {"ttl: 1h", "ttl: 48h"},
		"refresh floor over a day":    {"refreshFloor: 1m", "refreshFloor: 48h"},
		"refPrefix with a dot":        {"refPrefix: OB-BKG", "refPrefix: OB.BKG"},
		"refPrefix too long":          {"refPrefix: OB-BKG", "refPrefix: OB-BKG-0123456789012345678901234567890123"},
		"name with a control char":    {"name: Ocean Blue", "name: \"Ocean\\x07Blue\""},
		"name with a bidi override":   {"name: Ocean Blue", "name: \"Ocean Blue\\u202e\""},
		"listen with a word for port": {`listen: ":8443"`, `listen: "host:abc"`},
		"listen port out of range":    {`listen: ":8443"`, `listen: ":70000"`},
		"peer target with no host":    {"cymbal-shoes.example: cymbal-shoes:8443", `cymbal-shoes.example: ":8443"`},
		"self as a partner":           {"partners: [cymbal-shoes.example", "partners: [ocean-blue.example"},
		"self on the deny list":       {"deny: [mallory.example]", "deny: [ocean-blue.example]"},
	} {
		in := strings.Replace(operatorYAML, edit[0], edit[1], 1)
		if in == operatorYAML {
			t.Fatalf("%s: edit didn't apply", name)
		}
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	shipperWithOperator := strings.Replace(operatorYAML, "role: operator", "role: shipper", 1)
	if _, err := Parse([]byte(shipperWithOperator)); err == nil {
		t.Error("a shipper with an operator section was accepted")
	}
	if _, err := Parse([]byte("{")); err == nil {
		t.Error("invalid YAML accepted")
	}
}

// TestRoles: a node may have several roles; the one-role form still
// works; each role's section follows its role.
func TestRoles(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(operatorYAML, "role: operator", "roles: [operator, shipper]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Has(RoleOperator) || !c.Has(RoleShipper) || c.Has(RoleReceiver) || !c.Customer() || c.OneRole != "" {
		t.Errorf("roles %v (role %q)", c.Roles, c.OneRole)
	}
	if c, err := Parse([]byte(operatorYAML)); err != nil || !slices.Equal(c.Roles, []string{RoleOperator}) || c.Customer() {
		t.Errorf("one-role form: %v, %v", c, err)
	}
	// A carrier that also ships may keep private business context; a
	// carrier alone may not.
	both := strings.Replace(operatorYAML, "role: operator", "roles: [operator, shipper]", 1) + "business: { file: /etc/ulp/business.yaml }\n"
	if _, err := Parse([]byte(both)); err != nil {
		t.Errorf("operator and shipper with business: %v", err)
	}
	receiver := `{domain: cymbal-footwear.example, name: Cymbal Footwear, roles: [receiver], listen: ":8443", tls: {mode: insecure-dev},
		keys: [{kid: cf-1, privateKeyFile: /etc/ulp/cf.pem}], business: {file: /etc/ulp/business.yaml}}`
	if c, err := Parse([]byte(receiver)); err != nil || !c.Customer() || c.Has(RoleShipper) || c.Simulator != nil {
		t.Errorf("receiver: %+v, %v", c, err)
	}
}

// TestWeb: the UI's personas act in the node's roles, and demo sign-in
// (no password) is for local deployments only.
func TestWeb(t *testing.T) {
	web := "web:\n  auth: demo\n  personas:\n    - { id: sam, name: Sam, title: Ops desk, roles: [operator] }\n"
	local := strings.Replace(operatorYAML, `listen: ":8443"`, `listen: "127.0.0.1:8443"`, 1)
	c, err := Parse([]byte(local + web))
	if err != nil {
		t.Fatal(err)
	}
	if c.Web == nil || len(c.Web.Personas) != 1 || c.Web.Personas[0].Roles[0] != RoleOperator {
		t.Errorf("web %+v", c.Web)
	}
	ok := strings.Replace(web, "  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://8441-ws.example\", \"http://127.0.0.1:8441\", \"http://[::1]:8441\"]\n"+
		"  presenter: { panels: [{ name: Cymbal, url: \"http://127.0.0.1:8442/ui/\" }, { name: Ocean, url: \"https://8441-ws.example/ui/\" }] }\n", 1)
	if c, err := Parse([]byte(local + ok)); err != nil || len(c.Web.Presenter.Panels) != 2 || len(c.Web.FrameAncestors) != 3 {
		t.Errorf("a presenter: %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(local, "127.0.0.1:8443", "localhost:8443", 1) + web)); err != nil {
		t.Errorf("demo sign-in on localhost: %v", err)
	}
	// Demo sign-in has no password: the node must listen on loopback only.
	for _, listen := range []string{`":8443"`, `"0.0.0.0:8443"`, `"10.0.0.5:8443"`, `"[::]:8443"`} {
		if _, err := Parse([]byte(strings.Replace(operatorYAML, `listen: ":8443"`, "listen: "+listen, 1) + web)); err == nil {
			t.Errorf("demo sign-in on %s: accepted", listen)
		}
	}
	for name, edit := range map[string][2]string{
		"no auth":                              {"auth: demo\n", ""},
		"hosted auth, not yet":                 {"auth: demo", "auth: iap"},
		"no personas":                          {"  personas:\n    - { id: sam, name: Sam, title: Ops desk, roles: [operator] }\n", "  personas: []\n"},
		"a bad id":                             {"id: sam", "id: Sam!"},
		"a role the node lacks":                {"roles: [operator]", "roles: [shipper]"},
		"no role":                              {"roles: [operator]", "roles: []"},
		"an empty name":                        {"name: Sam", `name: ""`},
		"a control char in title":              {"title: Ops desk", `title: "Ops\u0007desk"`},
		"an uppercase host":                    {"  auth: demo\n", "  auth: demo\n  hosts: [Bad.Example]\n"},
		"a host with a port":                   {"  auth: demo\n", "  auth: demo\n  hosts: [\"8441-ws.example:443\"]\n"},
		"an ancestor with a path":              {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://p.example/ui\"]\n"},
		"an ancestor with a quote":             {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://p.example'\"]\n"},
		"an ancestor wildcard":                 {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://*\"]\n"},
		"an ftp ancestor":                      {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"ftp://p.example\"]\n"},
		"an ancestor ending in ?":              {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://p.example?\"]\n"},
		"an ancestor ending in #":              {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://p.example#\"]\n"},
		"an ancestor with an empty port":       {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://p.example:\"]\n"},
		"an ancestor port out of range":        {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://p.example:99999\"]\n"},
		"an ancestor port with a leading zero": {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://p.example:0080\"]\n"},
		"an unspecified ancestor":              {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"http://0.0.0.0:8441\"]\n"},
		"an ancestor with a space":             {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://p.example x\"]\n"},
		"an ancestor with a semicolon":         {"  auth: demo\n", "  auth: demo\n  frameAncestors: [\"https://p.example;x\"]\n"},
		"a panel with userinfo":                {"  auth: demo\n", "  auth: demo\n  presenter: { panels: [{ name: P, url: \"https://u@p.example/ui/\" }] }\n"},
		"a panel ending in ?":                  {"  auth: demo\n", "  auth: demo\n  presenter: { panels: [{ name: P, url: \"https://p.example/ui/?\" }] }\n"},
		"a panel not at /ui/":                  {"  auth: demo\n", "  auth: demo\n  presenter: { panels: [{ name: P, url: \"https://p.example/admin/\" }] }\n"},
		"a panel with a query":                 {"  auth: demo\n", "  auth: demo\n  presenter: { panels: [{ name: P, url: \"https://p.example/ui/?x=1\" }] }\n"},
		"a panel with no name":                 {"  auth: demo\n", "  auth: demo\n  presenter: { panels: [{ name: \"\", url: \"https://p.example/ui/\" }] }\n"},
		"a presenter of no panels":             {"  auth: demo\n", "  auth: demo\n  presenter: { panels: [] }\n"},
		"a duplicate id": {"    - { id: sam, name: Sam, title: Ops desk, roles: [operator] }\n",
			"    - { id: sam, name: Sam, title: Ops desk, roles: [operator] }\n    - { id: sam, name: Sammy, roles: [operator] }\n"},
	} {
		in := local + strings.Replace(web, edit[0], edit[1], 1)
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestLLMBackends: each provider by API key or on Vertex AI, with the
// usual environment variable names by default, and never a key in the
// file.
func TestLLMBackends(t *testing.T) {
	for in, want := range map[string]LLM{
		"{provider: anthropic}":                  {Provider: "anthropic", Backend: "api", APIKeyEnv: "ANTHROPIC_API_KEY"},
		"{provider: gemini, model: g}":           {Provider: "gemini", Backend: "api", Model: "g", APIKeyEnv: "GEMINI_API_KEY"},
		"{provider: anthropic, backend: vertex}": {Provider: "anthropic", Backend: "vertex", ProjectEnv: "ANTHROPIC_VERTEX_PROJECT_ID", LocationEnv: "CLOUD_ML_REGION"},
		"{provider: gemini, backend: vertex, model: gemini-3.8-flash}": {Provider: "gemini", Backend: "vertex", Model: "gemini-3.8-flash",
			ProjectEnv: "GOOGLE_CLOUD_PROJECT", LocationEnv: "GOOGLE_CLOUD_LOCATION"},
		"{provider: gemini, backend: vertex, model: g, projectEnv: P, locationEnv: L}": {Provider: "gemini", Backend: "vertex", Model: "g",
			ProjectEnv: "P", LocationEnv: "L"},
		"{provider: gemini, model: g, thinkingBudget: -1}": {Provider: "gemini", Backend: "api", Model: "g", APIKeyEnv: "GEMINI_API_KEY", ThinkingBudget: -1},
	} {
		c, err := Parse([]byte(strings.Replace(operatorYAML, "llm: { provider: fake, model: m, apiKeyEnv: LLM_API_KEY }", "llm: "+in, 1)))
		if err != nil || *c.LLM != want {
			t.Errorf("%s: %+v, %v", in, c.LLM, err)
		}
	}
	for _, in := range []string{
		"{provider: openai}",
		"{provider: anthropic, thinkingBudget: 100}",
		"{provider: gemini}",
		"{provider: gemini, backend: vertex}",
		"{provider: anthropic, model: ../../other}",
		"{provider: gemini, backend: azure}",
		"{provider: gemini, backend: vertex, apiKeyEnv: GEMINI_API_KEY}",
		"{provider: anthropic, projectEnv: P}",
		"{provider: gemini, backend: vertex, projectEnv: \"my project\"}",
	} {
		if _, err := Parse([]byte(strings.Replace(operatorYAML, "llm: { provider: fake, model: m, apiKeyEnv: LLM_API_KEY }", "llm: "+in, 1))); err == nil {
			t.Errorf("%s: accepted", in)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(p, []byte(operatorYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Errorf("Load: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
}

func TestSigningKeyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "key.pem")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := WriteSigningKey(p, priv); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key written with mode %v", info.Mode().Perm())
	}
	got, err := LoadSigningKey(p)
	if err != nil || !got.Equal(priv) {
		t.Fatalf("LoadSigningKey = %v", err)
	}
	if err := WriteSigningKey(p, priv); err == nil {
		t.Error("WriteSigningKey overwrote an existing file")
	}
	if err := WriteSigningKey(filepath.Join(dir, "no", "such", "dir"), priv); err == nil {
		t.Error("WriteSigningKey into a missing directory succeeded")
	}
}

func TestLoadSigningKeyRejects(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	good := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ec)

	for name, p := range map[string]string{
		"group-readable": write("group.pem", good, 0o640),
		"world-readable": write("world.pem", good, 0o604),
		"not PEM":        write("junk.pem", []byte("key"), 0o600),
		"wrong type":     write("type.pem", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}), 0o600),
		"trailing data":  write("trail.pem", append(append([]byte(nil), good...), []byte("extra")...), 0o600),
		"bad DER":        write("der.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2}}), 0o600),
		"not Ed25519":    write("ec.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}), 0o600),
		"missing":        filepath.Join(dir, "missing.pem"),
		"a directory":    dir,
	} {
		if _, err := LoadSigningKey(p); err == nil {
			t.Errorf("%s: LoadSigningKey succeeded", name)
		}
	}
	// A good key in a directory others can write to: they could swap it.
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(shared, "key.pem")
	if err := os.WriteFile(p, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigningKey(p); err == nil {
		t.Error("a key in a world-writable directory was accepted")
	}
	if _, err := LoadSigningKey(filepath.Join(t.TempDir(), "missing-dir", "key.pem")); err == nil {
		t.Error("a key in a missing directory was accepted")
	}
}

// TestGoogle: Google sign-in for the UI and for MCP, on a node that isn't
// on loopback, and everything it refuses.
func TestGoogle(t *testing.T) {
	base := strings.Replace(operatorYAML, "role: operator", "roles: [operator, shipper]", 1)
	google := "google:\n  clientID: 123-abc.apps.googleusercontent.com\n  clientSecretEnv: GOOGLE_CLIENT_SECRET\n  publicURL: https://ocean-blue.example\n"
	web := "web:\n  auth: google\n  personas:\n" +
		"    - { id: sam, name: Sam, roles: [operator], members: [sam@ocean-blue.example] }\n" +
		"    - { id: dana, name: Dana, roles: [shipper], members: [\"@ocean-blue.example\"] }\n"
	mcp := "mcp: { server: { auth: google } }\n"
	full := strings.Replace(base, "mcp: { connectors: [{ name: tms, url: \"https://tms.internal.ocean-blue.example/mcp\" }] }\n", "", 1) + google + web + mcp
	c, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if c.Google.ClientID != "123-abc.apps.googleusercontent.com" || c.MCP.Server.Auth != AuthGoogle || c.Web.Personas[1].Members[0] != "@ocean-blue.example" {
		t.Errorf("parsed %+v %+v", c.Google, c.MCP.Server)
	}
	// Localhost may try it out over plain http.
	if _, err := Parse([]byte(strings.Replace(full, "https://ocean-blue.example", "http://cymbal.localhost:8442", 1))); err != nil {
		t.Errorf("http on localhost: %v", err)
	}
	// The token is still the default for MCP.
	if c, err := Parse([]byte(strings.Replace(full, "auth: google } }", "tokenEnv: T } }", 1))); err != nil || c.MCP.Server.Auth != AuthToken {
		t.Errorf("mcp token beside google web sign-in: %v", err)
	}
	for name, edit := range map[string][2]string{
		"no google section":             {google, ""},
		"a bad client ID":               {"123-abc.apps", "evil.apps"},
		"no secret":                     {"  clientSecretEnv: GOOGLE_CLIENT_SECRET\n", ""},
		"two secrets":                   {"  clientSecretEnv: GOOGLE_CLIENT_SECRET\n", "  clientSecretEnv: G\n  clientSecretFile: /s\n"},
		"a secret env not a name":       {"clientSecretEnv: GOOGLE_CLIENT_SECRET", "clientSecretEnv: 'a b'"},
		"a relative secret file":        {"clientSecretEnv: GOOGLE_CLIENT_SECRET", "clientSecretFile: secret"},
		"plain http, not localhost":     {"https://ocean-blue.example", "http://ocean-blue.example"},
		"a public URL with a path":      {"https://ocean-blue.example", "https://ocean-blue.example/ui"},
		"no public URL":                 {"  publicURL: https://ocean-blue.example\n", ""},
		"a persona with no members":     {"members: [sam@ocean-blue.example]", "members: []"},
		"an uppercase member":           {"sam@ocean-blue.example", "Sam@ocean-blue.example"},
		"a member with no domain":       {"sam@ocean-blue.example", "sam@"},
		"a member that is a domain":     {"sam@ocean-blue.example", "ocean-blue.example"},
		"a member with a space":         {"sam@ocean-blue.example", "\"s m@ocean-blue.example\""},
		"a bare TLD domain":             {"\"@ocean-blue.example\"", "\"@example\""},
		"an unknown web auth":           {"  auth: google\n", "  auth: iap\n"},
		"an unknown mcp auth":           {"auth: google } }", "auth: basic } }"},
		"mcp google with a token":       {"auth: google } }", "auth: google, tokenEnv: T } }"},
		"mcp google, no shipper member": {"roles: [shipper], members", "roles: [operator], members"},
	} {
		in := strings.Replace(full, edit[0], edit[1], 1)
		if in == full {
			t.Fatalf("%s: the edit didn't apply", name)
		}
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A google section nothing uses is a mistake.
	demo := strings.NewReplacer("auth: google } }", "tokenEnv: T } }", "  auth: google\n", "  auth: demo\n", `listen: ":8443"`, `listen: "127.0.0.1:8443"`).Replace(full)
	if _, err := Parse([]byte(strings.Replace(demo, google, "", 1))); err != nil {
		t.Fatalf("the demo config without google: %v", err)
	}
	if _, err := Parse([]byte(demo)); err == nil {
		t.Error("an unused google section: accepted")
	}
}

// TestSigningKeyEnv: a key from the environment, which is then removed.
func TestSigningKeyEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "key.pem")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := WriteSigningKey(p, priv); err != nil {
		t.Fatal(err)
	}
	pemData, _ := os.ReadFile(p)
	t.Setenv("SUPERCARGO_TEST_KEY", string(pemData))
	got, err := LoadSigningKeyEnv("SUPERCARGO_TEST_KEY")
	if err != nil || !got.Equal(priv) {
		t.Fatalf("LoadSigningKeyEnv = %v", err)
	}
	if _, ok := os.LookupEnv("SUPERCARGO_TEST_KEY"); ok {
		t.Error("the key stayed in the environment")
	}
	if _, err := LoadSigningKeyEnv("SUPERCARGO_TEST_KEY"); err == nil {
		t.Error("an unset key loaded")
	}
	for name, v := range map[string]string{"empty": "", "not PEM": "key", "two blocks": string(pemData) + string(pemData)} {
		t.Setenv("SUPERCARGO_TEST_KEY", v)
		if _, err := LoadSigningKeyEnv("SUPERCARGO_TEST_KEY"); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// TestProxyAndKeyEnv: behind a proxy that serves https (Cloud Run), with
// the key from the environment.
func TestProxyAndKeyEnv(t *testing.T) {
	base := strings.NewReplacer(
		"tls: { mode: local-ca, certFile: /certs/ob.pem, keyFile: /certs/ob-key.pem, caFile: /certs/ca.pem }", "tls: { mode: proxy }",
		"privateKeyFile: /keys/ob-2026-1.pem", "privateKeyEnv: ULP_SIGNING_KEY").Replace(operatorYAML)
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.TLS.Mode != TLSProxy || c.Keys[0].PrivateKeyEnv != "ULP_SIGNING_KEY" {
		t.Errorf("parsed %+v %+v", c.TLS, c.Keys)
	}
	for name, edit := range map[string][2]string{
		"proxy with a certificate": {"tls: { mode: proxy }", "tls: { mode: proxy, certFile: /c.pem, keyFile: /k.pem }"},
		"a key from both":          {"privateKeyEnv: ULP_SIGNING_KEY", "privateKeyEnv: ULP_SIGNING_KEY\n    privateKeyFile: /k.pem"},
		"a key from neither":       {"privateKeyEnv: ULP_SIGNING_KEY", ""},
		"a key env not a name":     {"privateKeyEnv: ULP_SIGNING_KEY", "privateKeyEnv: 'a b'"},
		"an unknown TLS mode":      {"tls: { mode: proxy }", "tls: { mode: offload }"},
	} {
		in := strings.Replace(base, edit[0], edit[1], 1)
		if in == base {
			t.Fatalf("%s: the edit didn't apply", name)
		}
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Demo sign-in has no password: never behind a proxy that publishes it.
	demo := strings.Replace(base, `listen: ":8443"`, `listen: "127.0.0.1:8443"`, 1) +
		"web:\n  auth: demo\n  personas:\n    - { id: sam, name: Sam, roles: [operator] }\n"
	if _, err := Parse([]byte(demo)); err == nil {
		t.Error("demo sign-in behind a proxy: accepted")
	}
}
