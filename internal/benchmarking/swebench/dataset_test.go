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
	"strings"
	"testing"
)

const goodDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func manifest(ids ...string) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(`{"instance_id":"` + id + `","repo":"r","image":"reg/img@` + goodDigest + `","patch":"p","eval_script":"e"}` + "\n")
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
	got, err := Select(all, nil, 2)
	if err != nil || len(got) != 2 || got[1].InstanceID != "b" {
		t.Fatalf("limit: got %+v, %v", got, err)
	}
	got, err = Select(all, []string{"c", "a"}, 0)
	if err != nil || len(got) != 2 || got[0].InstanceID != "a" || got[1].InstanceID != "c" {
		t.Fatalf("ids keep manifest order: got %+v, %v", got, err)
	}
	got, err = Select(all, []string{"c", "a"}, 1)
	if err != nil || len(got) != 1 || got[0].InstanceID != "a" {
		t.Fatalf("ids then limit: got %+v, %v", got, err)
	}
	if _, err := Select(all, []string{"zz"}, 0); err == nil {
		t.Fatal("unknown id must error")
	}
}
