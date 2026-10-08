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

package llm

import (
	"fmt"
	"regexp"
)

var (
	vertexLocation = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	// A project ID, or a legacy domain-scoped one (example.com:proj).
	vertexProject = regexp.MustCompile(`^[a-z0-9][a-z0-9.:-]{0,99}$`)
)

// CheckVertex refuses a Vertex AI project or location that isn't one. Both
// come from the environment and end up in the endpoint's host and path,
// where anything else could send the OAuth token to another host.
func CheckVertex(project, location string) error {
	if !vertexProject.MatchString(project) {
		return fmt.Errorf("vertex: %q is not a project ID", project)
	}
	if !vertexLocation.MatchString(location) {
		return fmt.Errorf("vertex: %q is not a location", location)
	}
	return nil
}

// VertexEndpoint is Vertex AI's endpoint for a (checked) location: global,
// a multi-region (us, eu), or a region.
func VertexEndpoint(location string) string {
	switch location {
	case "global":
		return "https://aiplatform.googleapis.com/"
	case "us", "eu":
		return "https://aiplatform." + location + ".rep.googleapis.com/"
	}
	return "https://" + location + "-aiplatform.googleapis.com/"
}
