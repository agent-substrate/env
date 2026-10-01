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

package apiservice

import (
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/proto"
)

const (
	testGuestImage = "example.com/ate-env-guest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTaskImage  = "docker.io/library/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func modeABase() *ateapipb.ActorTemplate {
	return &ateapipb.ActorTemplate{
		Metadata:       &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "default-template", Uid: "u1", Version: 3},
		WorkerSelector: &ateapipb.Selector{MatchLabels: map[string]string{"workload": "default-env"}},
		Containers: []*ateapipb.Container{{
			Name:    "guest",
			Image:   testGuestImage,
			Command: []string{"/ko-app/ate-env-guest", "-workspace", "/workspace"},
			Env:     []*ateapipb.EnvVar{{Name: "PORT", Value: "80"}},
			Readyz:  &ateapipb.ContainerReadyz{HttpGet: &ateapipb.HTTPGetAction{Path: "/readyz", Port: 80}},
		}},
		SnapshotsConfig: &ateapipb.SnapshotsConfig{StorageLocation: "gs://b/p/"},
		SandboxConfig:   &ateapipb.SandboxConfig{SandboxClass: ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR, ConfigName: "gvisor-default"},
		Status:          &ateapipb.ActorTemplateStatus{},
	}
}

func TestImageDigestAndTemplateName(t *testing.T) {
	name, err := ImageTemplateName("default-template", testTaskImage)
	if err != nil {
		t.Fatal(err)
	}
	if name != "default-template-0123456789ab" {
		t.Errorf("name = %q", name)
	}
	for _, bad := range []string{"python:3.12", "python@sha256:abc", "python@sha512:" + strings.Repeat("a", 128), ""} {
		if _, err := ImageDigest(bad); err == nil {
			t.Errorf("ImageDigest(%q) accepted an unpinned reference", bad)
		}
	}
	// The derived name must stay a valid resource name.
	if _, err := ImageTemplateName(strings.Repeat("b", 51), testTaskImage); err == nil {
		t.Error("a 64-character derived name was accepted")
	}
	if name, err := ImageTemplateName(strings.Repeat("b", 50), testTaskImage); err != nil || len(name) != 63 {
		t.Errorf("a 63-character derived name should be accepted: %q, %v", name, err)
	}
	if _, err := ImageTemplateName("Default", testTaskImage); err == nil {
		t.Error("an uppercase base name was accepted")
	}
}

func TestDeriveImageTemplateFromGuestImageBase(t *testing.T) {
	base := modeABase()
	before := proto.Clone(base).(*ateapipb.ActorTemplate)

	got, err := DeriveImageTemplate(base, "default-template-0123456789ab", testTaskImage)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(base, before) {
		t.Error("DeriveImageTemplate mutated the base template")
	}

	md := got.GetMetadata()
	if md.GetName() != "default-template-0123456789ab" || md.GetAtespace() != "ate-env" {
		t.Errorf("metadata = %v", md)
	}
	if md.GetUid() != "" || md.GetVersion() != 0 || got.GetStatus() != nil {
		t.Errorf("server-assigned fields were copied: uid=%q version=%d status=%v", md.GetUid(), md.GetVersion(), got.GetStatus())
	}

	c := got.GetContainers()[0]
	if c.GetImage() != testTaskImage {
		t.Errorf("container image = %q, want the task image", c.GetImage())
	}
	wantCmd := []string{"/ate/ko-app/ate-env-guest", "-workspace", "/workspace"}
	if strings.Join(c.GetCommand(), " ") != strings.Join(wantCmd, " ") {
		t.Errorf("command = %v, want %v", c.GetCommand(), wantCmd)
	}
	if len(got.GetVolumes()) != 1 || got.GetVolumes()[0].GetName() != GuestVolumeName ||
		got.GetVolumes()[0].GetImage().GetReference() != testGuestImage {
		t.Errorf("volumes = %v, want one image volume %q with the guest image", got.GetVolumes(), GuestVolumeName)
	}
	if len(c.GetVolumeMounts()) != 1 || c.GetVolumeMounts()[0].GetName() != GuestVolumeName ||
		c.GetVolumeMounts()[0].GetMountPath() != GuestMountPath {
		t.Errorf("volume mounts = %v, want %s at %s", c.GetVolumeMounts(), GuestVolumeName, GuestMountPath)
	}
	// Everything else carries over.
	if !proto.Equal(got.GetWorkerSelector(), base.GetWorkerSelector()) ||
		!proto.Equal(got.GetSnapshotsConfig(), base.GetSnapshotsConfig()) ||
		!proto.Equal(got.GetSandboxConfig(), base.GetSandboxConfig()) ||
		!proto.Equal(c.GetReadyz(), base.GetContainers()[0].GetReadyz()) ||
		len(c.GetEnv()) != 1 {
		t.Error("worker selector, snapshots, sandbox config, readyz or env did not carry over")
	}
}

func TestDeriveImageTemplateFromInjectedBase(t *testing.T) {
	// A base that already mounts the guest: only the container image changes.
	base, err := DeriveImageTemplate(modeABase(), "py-base", "example.com/base@sha256:"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	got, err := DeriveImageTemplate(base, "py-base-0123456789ab", testTaskImage)
	if err != nil {
		t.Fatal(err)
	}
	c := got.GetContainers()[0]
	if c.GetImage() != testTaskImage {
		t.Errorf("image = %q", c.GetImage())
	}
	if c.GetCommand()[0] != "/ate/ko-app/ate-env-guest" {
		t.Errorf("command re-rooted twice: %v", c.GetCommand())
	}
	if len(got.GetVolumes()) != 1 || len(c.GetVolumeMounts()) != 1 {
		t.Errorf("guest volume duplicated: volumes=%d mounts=%d", len(got.GetVolumes()), len(c.GetVolumeMounts()))
	}
}

func TestDeriveImageTemplateDefaultsCommand(t *testing.T) {
	base := modeABase()
	base.Containers[0].Command = nil
	got, err := DeriveImageTemplate(base, "x", testTaskImage)
	if err != nil {
		t.Fatal(err)
	}
	if cmd := got.GetContainers()[0].GetCommand(); len(cmd) != 1 || cmd[0] != "/ate/ko-app/ate-env-guest" {
		t.Errorf("command = %v", cmd)
	}
}

func TestDeriveImageTemplateRejects(t *testing.T) {
	if _, err := DeriveImageTemplate(modeABase(), "x", "python:3.12"); err == nil {
		t.Error("unpinned task image accepted")
	}
	unpinnedGuest := modeABase()
	unpinnedGuest.Containers[0].Image = "example.com/ate-env-guest:latest"
	if _, err := DeriveImageTemplate(unpinnedGuest, "x", testTaskImage); err == nil {
		t.Error("unpinned guest image accepted as an image volume")
	}
	if _, err := DeriveImageTemplate(&ateapipb.ActorTemplate{Metadata: &ateapipb.ResourceMetadata{Name: "empty"}}, "x", testTaskImage); err == nil {
		t.Error("base without containers accepted")
	}
	if _, err := DeriveImageTemplate(modeABase(), "", testTaskImage); err == nil {
		t.Error("empty name accepted")
	}
	wrapped := modeABase()
	wrapped.Containers[0].Command = []string{"sh", "-c", "exec /ko-app/ate-env-guest"}
	if _, err := DeriveImageTemplate(wrapped, "x", testTaskImage); err == nil || !strings.Contains(err.Error(), "re-root") {
		t.Errorf("a shell-wrapped base command must be rejected, got %v", err)
	}
}
