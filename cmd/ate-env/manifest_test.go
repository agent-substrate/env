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

import (
	"bytes"
	"strings"
	"testing"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func testManifestConfig() manifestConfig {
	return manifestConfig{
		namespace:   "ate-env",
		template:    "default-template",
		workerPool:  "default-template-workerpool",
		workerImage: "example.com/ateom@sha256:bbbb",
		apiImage:    "example.com/api@sha256:cccc",
		replicas:    3,
		apiReplicas: 1,
		apiPort:     7777,
	}
}

func testTemplateConfig() templateConfig {
	return templateConfig{
		template:        "default-template",
		atespace:        "ate-env",
		guestImage:      "example.com/guest@sha256:aaaa",
		snapshotsBucket: "gs://bucket/ate-env/",
		guestCommand:    []string{"/ko-app/ate-env-guest"},
	}
}

func TestManifestConfigResolveImages(t *testing.T) {
	cfg := testManifestConfig()
	if err := cfg.resolveImages(); err != nil {
		t.Errorf("resolveImages with images set: %v", err)
	}
	cfg.apiImage = ""
	if err := cfg.resolveImages(); err == nil {
		t.Error("resolveImages with a missing api image: want error, got nil")
	}
	cfg = testManifestConfig()
	cfg.workerImage = ""
	if err := cfg.resolveImages(); err == nil {
		t.Error("resolveImages with a missing worker image: want error, got nil")
	}
}

func TestTemplateConfigResolveImages(t *testing.T) {
	cfg := testTemplateConfig()
	if err := cfg.resolveImages(); err != nil {
		t.Errorf("resolveImages with template images and bucket set: %v", err)
	}
	cfg.guestImage = ""
	if err := cfg.resolveImages(); err == nil {
		t.Error("resolveImages with a missing guest image: want error, got nil")
	}
	cfg = testTemplateConfig()
	cfg.snapshotsBucket = ""
	if err := cfg.resolveImages(); err == nil {
		t.Error("resolveImages with a missing snapshots bucket: want error, got nil")
	}
}

func TestBuildManifests(t *testing.T) {
	cfg := testManifestConfig()
	objs := buildManifests(cfg)
	if len(objs) != 4 {
		t.Fatalf("got %d manifests, want 4", len(objs))
	}

	ns := objs[0].(*corev1.Namespace)
	if ns.Name != "ate-env" {
		t.Errorf("namespace = %q, want ate-env", ns.Name)
	}

	pool := objs[1].(*atev1alpha1.WorkerPool)
	if pool.Namespace != cfg.namespace || pool.Name != "default-template-workerpool" {
		t.Errorf("workerpool = %s/%s, want %s/default-template-workerpool", pool.Namespace, pool.Name, cfg.namespace)
	}
	if pool.Spec.Replicas != 3 || pool.Spec.WorkerImage != cfg.workerImage {
		t.Errorf("workerpool spec = %+v, want replicas 3 and worker image %q", pool.Spec, cfg.workerImage)
	}
	if len(pool.Labels) != 0 {
		t.Errorf("workerpool labels = %v, want empty", pool.Labels)
	}
	if pool.Spec.Template != nil {
		t.Errorf("workerpool template = %v, want nil", pool.Spec.Template)
	}

	deployment := objs[2].(*appsv1.Deployment)
	containers := deployment.Spec.Template.Spec.Containers
	if len(containers) != 1 || containers[0].Image != cfg.apiImage {
		t.Errorf("api containers = %+v, want one container with image %q", containers, cfg.apiImage)
	}
	if *deployment.Spec.Replicas != 1 {
		t.Errorf("api replicas = %d, want 1", *deployment.Spec.Replicas)
	}

	service := objs[3].(*corev1.Service)
	if service.Spec.Selector["app"] != apiName {
		t.Errorf("api service selector = %v, want app=%s", service.Spec.Selector, apiName)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 7777 || service.Spec.Ports[0].TargetPort.IntValue() != 7777 {
		t.Errorf("api service ports = %+v, want 7777 -> 7777", service.Spec.Ports)
	}
}

func TestBuildActorTemplate(t *testing.T) {
	cfg := testTemplateConfig()
	var template *ateapipb.ActorTemplate = buildActorTemplate(cfg)

	if template.GetMetadata().GetName() != "default-template" {
		t.Errorf("template name = %q, want default-template", template.GetMetadata().GetName())
	}
	if template.GetMetadata().GetAtespace() != "ate-env" {
		t.Errorf("template atespace = %q, want ate-env", template.GetMetadata().GetAtespace())
	}
	if len(template.Containers) != 1 || template.Containers[0].Image != cfg.guestImage {
		t.Errorf("template containers = %+v, want one guest container with image %q",
			template.Containers, cfg.guestImage)
	}
	if template.GetSnapshotsConfig().GetStorageLocation() != cfg.snapshotsBucket {
		t.Errorf("snapshots location = %q, want %q", template.GetSnapshotsConfig().GetStorageLocation(), cfg.snapshotsBucket)
	}
	if template.GetWorkerSelector() != nil {
		t.Errorf("worker selector = %v, want nil", template.WorkerSelector)
	}
	readyz := template.Containers[0].Readyz
	if readyz == nil || readyz.GetHttpGet() == nil || readyz.GetHttpGet().Path != "/readyz" {
		t.Errorf("readyz = %+v, want HTTP GET /readyz", readyz)
	}
}

func TestBuildManifestsCustomAPIPort(t *testing.T) {
	cfg := testManifestConfig()
	cfg.apiPort = 9999
	objs := buildManifests(cfg)

	deployment := objs[2].(*appsv1.Deployment)
	container := deployment.Spec.Template.Spec.Containers[0]
	if got := container.Args[1]; got != "0.0.0.0:9999" {
		t.Errorf("api -listen arg = %q, want 0.0.0.0:9999", got)
	}
	if container.Ports[0].ContainerPort != 9999 {
		t.Errorf("api container port = %d, want 9999", container.Ports[0].ContainerPort)
	}
	if got := container.ReadinessProbe.HTTPGet.Port.IntValue(); got != 9999 {
		t.Errorf("api probe port = %d, want 9999", got)
	}

	service := objs[3].(*corev1.Service)
	if service.Spec.Ports[0].Port != 9999 || service.Spec.Ports[0].TargetPort.IntValue() != 9999 {
		t.Errorf("api service ports = %+v, want 9999 -> 9999", service.Spec.Ports)
	}
}

func TestWriteManifests(t *testing.T) {
	var buf bytes.Buffer
	if err := writeManifests(&buf, buildManifests(testManifestConfig())); err != nil {
		t.Fatalf("writeManifests: %v", err)
	}
	out := buf.String()

	if got := strings.Count(out, "\n---\n"); got != 3 {
		t.Errorf("got %d document separators, want 3:\n%s", got, out)
	}
	for _, want := range []string{
		"kind: Namespace",
		"kind: WorkerPool",
		"kind: Deployment",
		"kind: Service",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "status") {
		t.Errorf("output should not contain status fields:\n%s", out)
	}
	if strings.Contains(out, "{}") {
		t.Errorf("output should not contain empty map literals ({}):\n%s", out)
	}
}

func TestWriteActorTemplate(t *testing.T) {
	var buf bytes.Buffer
	if err := writeActorTemplate(&buf, buildActorTemplate(testTemplateConfig())); err != nil {
		t.Fatalf("writeActorTemplate: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"name: default-template",
		"image: example.com/guest@sha256:aaaa",
		"storageLocation: gs://bucket/ate-env/",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "status") {
		t.Errorf("output should not contain status fields:\n%s", out)
	}
	if strings.Contains(out, "pauseImage") {
		t.Errorf("output should not contain pauseImage field:\n%s", out)
	}
	if strings.Contains(out, "onResume") {
		t.Errorf("output should not contain empty onResume field:\n%s", out)
	}
	if strings.Contains(out, "{}") {
		t.Errorf("output should not contain empty map literals ({}):\n%s", out)
	}
}

func TestManifestCommandExecution(t *testing.T) {
	cmd := newManifestCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"--api-image=example.com/api@sha256:cccc",
		"--worker-image=example.com/worker@sha256:bbbb",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute manifest: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "kind: Deployment") || !strings.Contains(out, "kind: WorkerPool") {
		t.Errorf("expected deployment and workerpool in output:\n%s", out)
	}
}

func TestManifestTemplateCommandExecution(t *testing.T) {
	cmd := newManifestCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"template",
		"--guest-image=example.com/guest@sha256:aaaa",
		"--snapshots-bucket=gs://bucket/prefix/",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute manifest template: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "image: example.com/guest@sha256:aaaa") || !strings.Contains(out, "storageLocation: gs://bucket/prefix/") {
		t.Errorf("expected guest image and storage location in output:\n%s", out)
	}
}
