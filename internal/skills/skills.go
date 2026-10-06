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

// Package skills loads agent skills (docs/DESIGN.md, A.10): SKILL.md files,
// YAML frontmatter (name, description) followed by Markdown instructions
// that steer a model.
//
// Built-in skills ship with the binary, one per interaction, versioned
// with the protocol. Company skills load from a directory in the node's
// own configuration and customize behavior. Both shape what a model says,
// nothing more: they add no tools, widen no data scope and relax no rule,
// because authorization, data loading and output validation happen in
// code, whatever a prompt says.
package skills

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Limits on a skill, and on a company's directory of them.
const (
	MaxDescription = 1024
	MaxBody        = 32 << 10
	MaxSkills      = 32
	MaxTotal       = 128 << 10 // all of a directory's skills together
	maxFile        = MaxBody + MaxDescription + 4096
)

// readCapped reads a SKILL.md, which must be a regular file, reading at most
// maxFile+1 bytes whatever its size claimed when opened.
func readCapped(fsys fs.FS, path string) ([]byte, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, fmt.Errorf("skills: %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("skills: %s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("skills: %s: %w", path, err)
	}
	if len(data) > maxFile {
		return nil, fmt.Errorf("skills: %s is over %d bytes", path, maxFile)
	}
	return data, nil
}

// Skill is one agent skill.
type Skill struct {
	Name        string
	Description string
	Body        string
	// Source is "builtin" or "company".
	Source string
	// Hash identifies the file's content in logs: the first 12 hex digits
	// of its SHA-256.
	Hash string
}

// ID is name@hash, how logs name a skill a model ran with.
func (s Skill) ID() string { return s.Name + "@" + s.Hash }

var namePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Parse reads a SKILL.md file.
func Parse(data []byte, source string) (Skill, error) {
	if len(data) > maxFile {
		return Skill{}, fmt.Errorf("skills: file is %d bytes, too large", len(data))
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Skill{}, errors.New("skills: no YAML frontmatter (the file must start with ---)")
	}
	front, body, ok := strings.Cut(rest, "\n---\n")
	if after, empty := strings.CutPrefix(rest, "---\n"); empty {
		front, body, ok = "", after, true // empty frontmatter: reported as no name below
	}
	if !ok {
		front, ok = strings.CutSuffix(rest, "\n---")
		if !ok {
			return Skill{}, errors.New("skills: the frontmatter is not closed with ---")
		}
		body = ""
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	// Other frontmatter fields (license, metadata, ...) are allowed and
	// ignored.
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		return Skill{}, fmt.Errorf("skills: frontmatter: %w", err)
	}
	body = strings.TrimSpace(body)
	switch {
	case len(meta.Name) > 64 || !namePattern.MatchString(meta.Name):
		return Skill{}, fmt.Errorf("skills: name %q is not 1-64 lowercase letters, digits and hyphens", meta.Name)
	case meta.Description == "" || utf8.RuneCountInString(meta.Description) > MaxDescription:
		return Skill{}, fmt.Errorf("skills: %s: description must be 1-%d characters", meta.Name, MaxDescription)
	case len(body) > MaxBody:
		return Skill{}, fmt.Errorf("skills: %s: body is over %d bytes", meta.Name, MaxBody)
	}
	// What steers a model must be all there is to read: no control or
	// format characters (bidi overrides, zero-width, tag characters).
	for _, field := range []string{meta.Description, body} {
		if i := strings.IndexFunc(field, hidden); i >= 0 {
			r, _ := utf8.DecodeRuneInString(field[i:])
			return Skill{}, fmt.Errorf("skills: %s contains %U, which a skill may not", meta.Name, r)
		}
	}
	sum := sha256.Sum256(data)
	return Skill{Name: meta.Name, Description: strings.TrimSpace(meta.Description), Body: body, Source: source,
		Hash: hex.EncodeToString(sum[:])[:12]}, nil
}

func hidden(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) || r == 0x2028 || r == 0x2029 || r == utf8.RuneError
}

//go:embed builtin/*/SKILL.md
var builtinFS embed.FS

// Builtin returns the built-in skills, sorted by name.
func Builtin() []Skill {
	out, err := load(builtinFS, "builtin", "builtin")
	if err != nil {
		panic(err) // a broken built-in skill is a build defect; the tests catch it
	}
	return out
}

// LoadDir loads a company's skills: one subdirectory per skill, each with
// a SKILL.md. A skill that doesn't parse is an error, not skipped: a
// company's configuration that is silently ignored is worse than one that
// fails to load.
func LoadDir(dir string) ([]Skill, error) {
	return load(os.DirFS(dir), ".", "company")
}

func load(fsys fs.FS, root, source string) ([]Skill, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("skills: %w", err)
	}
	var out []Skill
	total := 0
	for _, e := range entries {
		// Plain files (a README) are not skills. Anything else that isn't a
		// skill directory is an error rather than skipped: a symlinked
		// directory, or one without a SKILL.md, is a skill someone meant
		// to load.
		if e.Type().IsRegular() {
			continue
		}
		if !e.IsDir() {
			return nil, fmt.Errorf("skills: %s is not a directory (symlinks aren't followed)", e.Name())
		}
		path := filepath.ToSlash(filepath.Join(root, e.Name(), "SKILL.md"))
		data, err := readCapped(fsys, path)
		if err != nil {
			return nil, err
		}
		s, err := Parse(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), source)
		if err != nil {
			return nil, fmt.Errorf("%w (%s)", err, path)
		}
		if s.Name != e.Name() {
			return nil, fmt.Errorf("skills: %s is named %q; a skill's directory must carry its name", path, s.Name)
		}
		out = append(out, s)
		if len(out) > MaxSkills {
			return nil, fmt.Errorf("skills: more than %d skills", MaxSkills)
		}
		// Every skill goes into each model call's prompt.
		if total += len(s.Body) + len(s.Description); total > MaxTotal {
			return nil, fmt.Errorf("skills: over %d bytes in all", MaxTotal)
		}
	}
	slices.SortFunc(out, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Find returns the skill named name.
func Find(all []Skill, name string) (Skill, bool) {
	for _, s := range all {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}
