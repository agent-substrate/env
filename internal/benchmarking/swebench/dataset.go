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

package swebench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// Instance is one SWE-bench task: the digest-pinned task image and the
// material a trajectory needs to evaluate it. The manifest is produced by
// benchmarking/swebench/make-dataset.py, one JSON object per line.
type Instance struct {
	InstanceID string `json:"instance_id"`
	Repo       string `json:"repo"`
	// Image is the task image, pinned by digest (repo@sha256:...). Substrate
	// requires pinned images because changing the image invalidates snapshots.
	Image string `json:"image"`
	// Patch is the gold patch; applying it makes FailToPass pass.
	Patch string `json:"patch"`
	// TestPatch adds or changes the tests that check the fix.
	TestPatch string `json:"test_patch"`
	// EvalScript is the SWE-bench harness eval script for this instance: it
	// applies TestPatch, runs the repo's test command and restores the test
	// files. Exit code 0 means every selected test passed.
	EvalScript string   `json:"eval_script"`
	FailToPass []string `json:"fail_to_pass"`
	PassToPass []string `json:"pass_to_pass"`
	// LogParser names the SWE-bench harness parser for this repo's test
	// output (parse_log_pytest, parse_log_django, ...); EvalType is
	// pass_and_fail or fail_only. Both come from the dataset row.
	LogParser string `json:"log_parser"`
	EvalType  string `json:"eval_type"`
}

var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// Validate reports the first thing that would make the instance unusable.
func (in Instance) Validate() error {
	switch {
	case in.InstanceID == "":
		return errors.New("instance_id is required")
	case in.Image == "":
		return fmt.Errorf("%s: image is required", in.InstanceID)
	case !digestPinned.MatchString(in.Image):
		return fmt.Errorf("%s: image %q is not digest-pinned", in.InstanceID, in.Image)
	case in.EvalScript == "":
		return fmt.Errorf("%s: eval_script is required", in.InstanceID)
	case in.LogParser == "":
		return fmt.Errorf("%s: log_parser is required", in.InstanceID)
	}
	return nil
}

// datasetFetchTimeout bounds a manifest download; a stalled server must not
// hang the loader before it starts.
const datasetFetchTimeout = 2 * time.Minute

// LoadDataset reads a JSONL manifest from a local path or an https URL. The
// manifest carries scripts that run inside the sandboxes, so a remote source
// must be https.
func LoadDataset(source string) ([]Instance, error) {
	var r io.ReadCloser
	if strings.HasPrefix(source, "http://") {
		return nil, fmt.Errorf("dataset %s: remote manifests must use https", source)
	}
	if strings.HasPrefix(source, "https://") {
		client := &http.Client{
			Timeout: datasetFetchTimeout,
			// The https rule holds for each hop: a redirect to http would
			// fetch the scripts in clear text.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Scheme != "https" {
					return fmt.Errorf("redirect to %s: remote manifests must use https", req.URL.Redacted())
				}
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return nil
			},
		}
		resp, err := client.Get(source)
		if err != nil {
			return nil, fmt.Errorf("fetch dataset: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch dataset: HTTP %d", resp.StatusCode)
		}
		r = resp.Body
	} else {
		f, err := os.Open(source)
		if err != nil {
			return nil, fmt.Errorf("open dataset: %w", err)
		}
		r = f
	}
	defer r.Close()
	return ParseDataset(r)
}

// ParseDataset decodes a JSONL manifest. Blank lines are skipped; every
// record must validate and instance ids must be unique.
func ParseDataset(r io.Reader) ([]Instance, error) {
	sc := bufio.NewScanner(r)
	// Eval scripts and patches run to hundreds of KiB for some repos.
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var out []Instance
	seen := map[string]bool{}
	line := 0
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var in Instance
		if err := json.Unmarshal(b, &in); err != nil {
			return nil, fmt.Errorf("dataset line %d: %w", line, err)
		}
		if err := in.Validate(); err != nil {
			return nil, fmt.Errorf("dataset line %d: %w", line, err)
		}
		if seen[in.InstanceID] {
			return nil, fmt.Errorf("dataset line %d: duplicate instance %s", line, in.InstanceID)
		}
		seen[in.InstanceID] = true
		out = append(out, in)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read dataset: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("dataset is empty")
	}
	return out, nil
}

// Select narrows a dataset: exclude drops the named instances first (a
// known-bad list; unknown names are ignored), ids then keeps only the named
// instances (in manifest order) and errors on an unknown or excluded id, and
// limit > 0 keeps the first limit of what is left.
func Select(all []Instance, ids, exclude []string, limit int) ([]Instance, error) {
	out := all
	drop := map[string]bool{}
	if len(exclude) > 0 {
		for _, id := range exclude {
			drop[id] = true
		}
		out = nil
		for _, in := range all {
			if !drop[in.InstanceID] {
				out = append(out, in)
			}
		}
		all = out
	}
	if len(ids) > 0 {
		for _, id := range ids {
			if drop[id] {
				return nil, fmt.Errorf("instance %s is on the exclude list", id)
			}
		}
		want := map[string]bool{}
		for _, id := range ids {
			want[id] = true
		}
		out = nil
		for _, in := range all {
			if want[in.InstanceID] {
				out = append(out, in)
				delete(want, in.InstanceID)
			}
		}
		if len(want) > 0 {
			missing := make([]string, 0, len(want))
			for id := range want {
				missing = append(missing, id)
			}
			return nil, fmt.Errorf("instances not in dataset: %s", strings.Join(missing, ", "))
		}
	}
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// ReadIDList reads instance ids from a file, one per line. Blank lines and
// lines starting with '#' are skipped, and anything after the first
// whitespace on a line is a comment, so a list can say why each id is there.
func ReadIDList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ids []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ids = append(ids, strings.Fields(line)[0])
	}
	return ids, sc.Err()
}
