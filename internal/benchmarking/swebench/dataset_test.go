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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func manifest(ids ...string) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(`{"instance_id":"` + id + `","repo":"r","image":"reg/img@` + goodDigest + `","patch":"p","eval_script":"e","log_parser":"parse_log_pytest"}` + "\n")
	}
	return b.String()
}

func TestParseDataset(t *testing.T) {
	got, err := ParseDataset(strings.NewReader("\n" + manifest("a__a-1", "b__b-2") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].InstanceID != "a__a-1" || got[1].InstanceID != "b__b-2" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseDatasetRejects(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"bad json":    "{not json}\n",
		"no id":       `{"image":"reg/img@` + goodDigest + `"}` + "\n",
		"no image":    `{"instance_id":"x"}` + "\n",
		"tag not pin": `{"instance_id":"x","image":"reg/img:latest"}` + "\n",
		"duplicate":   manifest("a", "a"),
		"no eval":     `{"instance_id":"x","image":"reg/img@` + goodDigest + `","log_parser":"parse_log_pytest"}` + "\n",
		"no parser":   `{"instance_id":"x","image":"reg/img@` + goodDigest + `","eval_script":"e"}` + "\n",
	}
	for name, in := range cases {
		if _, err := ParseDataset(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSelect(t *testing.T) {
	all, err := ParseDataset(strings.NewReader(manifest("a", "b", "c")))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Select(all, nil, nil, 2)
	if err != nil || len(got) != 2 || got[1].InstanceID != "b" {
		t.Fatalf("limit: got %+v, %v", got, err)
	}
	got, err = Select(all, []string{"c", "a"}, nil, 0)
	if err != nil || len(got) != 2 || got[0].InstanceID != "a" || got[1].InstanceID != "c" {
		t.Fatalf("ids keep manifest order: got %+v, %v", got, err)
	}
	got, err = Select(all, []string{"c", "a"}, nil, 1)
	if err != nil || len(got) != 1 || got[0].InstanceID != "a" {
		t.Fatalf("ids then limit: got %+v, %v", got, err)
	}
	if _, err := Select(all, []string{"zz"}, nil, 0); err == nil {
		t.Fatal("unknown id must error")
	}
}

func TestSelectExcludeAndIDList(t *testing.T) {
	all, err := ParseDataset(strings.NewReader(manifest("a", "b", "c")))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Select(all, nil, []string{"b", "unknown"}, 0)
	if err != nil || len(got) != 2 || got[0].InstanceID != "a" || got[1].InstanceID != "c" {
		t.Fatalf("exclude: got %+v, %v", got, err)
	}
	if _, err := Select(all, []string{"b"}, []string{"b"}, 0); err == nil || !strings.Contains(err.Error(), "exclude list") {
		t.Fatalf("an excluded id asked for by name must say so: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ids.txt")
	if err := os.WriteFile(path, []byte("# known network-dependent\nb needs pypi\n\n  c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := ReadIDList(path)
	if err != nil || len(ids) != 2 || ids[0] != "b" || ids[1] != "c" {
		t.Fatalf("ReadIDList: %v, %v", ids, err)
	}
}

func TestLoadDatasetRefusesPlainHTTP(t *testing.T) {
	if _, err := LoadDataset("http://example.invalid/manifest.jsonl"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http must be refused: %v", err)
	}
}
