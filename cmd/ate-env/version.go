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

package main

// Set at build time with -ldflags "-X main.version=... -X main.defaultAPIImage=...
// -X main.defaultGuestImage=...". Release binaries carry the digests of the
// images published with the same release, so `ate-env manifest` and
// `ate-env manifest template` work without naming them; a build from source
// leaves them empty and the flags required. See docs/release.md.
var (
	version           = "dev"
	defaultAPIImage   string
	defaultGuestImage string
)

// imageDefaultHelp describes a flag's release default, or the fact that a
// source build has none.
func imageDefaultHelp(what, def string) string {
	if def == "" {
		return what + " (required: this ate-env was built from source; release binaries default to the image published with the release, see docs/release.md)"
	}
	return what + ", pre-set to the image published with this release"
}
