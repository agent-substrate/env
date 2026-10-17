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
	"io"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/apiservice"
	atev1alpha1 "go.ate.dev/substrate/pkg/api/v1alpha1"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
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
	if got := ns.Labels[apiAccessLabel]; got != "true" {
		t.Errorf("namespace label %s = %q, want \"true\" (ate-system admits API clients by it)", apiAccessLabel, got)
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
	if len(pool.Spec.SandboxClasses) != 1 {
		t.Fatalf("workerpool sandboxClasses = %+v, want exactly one", pool.Spec.SandboxClasses)
	}
	sc := pool.Spec.SandboxClasses[0]
	if sc.Name != atev1alpha1.SandboxClassGvisor || sc.ConfigRef == nil || sc.ConfigRef.Name != sandboxConfigName {
		t.Errorf("workerpool sandboxClasses[0] = %+v, want gvisor with configRef %q", sc, sandboxConfigName)
	}
	// The pool's class and the template's are the same string since
	// substrate#2586 made the proto field a string. A pool of the wrong class
	// never hosts the template's actors.
	tmplClass := buildActorTemplate(testTemplateConfig()).GetSandboxConfig().GetSandboxClass()
	if tmplClass != string(sc.Name) {
		t.Errorf("workerpool sandbox class %q != template sandbox class %q", sc.Name, tmplClass)
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
	if template.GetSnapshotConfig().GetStorageLocation() != cfg.snapshotsBucket {
		t.Errorf("snapshots location = %q, want %q", template.GetSnapshotConfig().GetStorageLocation(), cfg.snapshotsBucket)
	}
	if got := template.GetSnapshotConfig().GetPreferredFidelity(); got != ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		t.Errorf("preferred fidelity = %v, want SNAPSHOT_FIDELITY_MEMORY", got)
	}
	if got := template.GetSnapshotConfig().GetGoldenSnapshotConfig(); got != nil {
		t.Errorf("golden snapshot config = %v, want nil (omitted means enabled)", got)
	}
	if got := template.GetSandboxConfig().GetConfigName(); got != sandboxConfigName {
		t.Errorf("sandbox config name = %q, want %q (must match the WorkerPool configRef)", got, sandboxConfigName)
	}
	if template.GetWorkerSelector() != nil {
		t.Errorf("worker selector = %v, want nil", template.WorkerSelector)
	}
	probe := template.Containers[0].GetWakeupProbe()
	if probe.GetHttpGet() == nil || probe.GetHttpGet().GetPath() != "/readyz" || probe.GetHttpGet().GetPort() != 80 {
		t.Errorf("wakeupProbe = %+v, want HTTP GET /readyz on port 80", probe)
	}
	// Substrate rejects an ActorTemplate without both limits (substrate#2285).
	if got := limits(template); got["cpu"] != defaultActorCPU || got["memory"] != defaultActorMemory || len(got) != 2 {
		t.Errorf("resources.limits = %v, want cpu=%s memory=%s", got, defaultActorCPU, defaultActorMemory)
	}
	if got := template.GetSandboxConfig().GetSandboxClass(); got != "gvisor" {
		t.Errorf("sandbox class = %q, want gvisor (Substrate accepts gvisor or microvm)", got)
	}
}

func limits(tmpl *ateapipb.ActorTemplate) map[string]string {
	m := map[string]string{}
	for _, l := range tmpl.GetResources().GetLimits() {
		m[l.GetName()] = l.GetQuantity()
	}
	return m
}

func TestBuildActorTemplateResources(t *testing.T) {
	cfg := testTemplateConfig()
	cfg.cpu, cfg.memory = "500m", "1Gi"
	if got := limits(buildActorTemplate(cfg)); got["cpu"] != "500m" || got["memory"] != "1Gi" {
		t.Errorf("resources.limits = %v, want cpu=500m memory=1Gi", got)
	}
}

func TestTemplateConfigRejectsBadResources(t *testing.T) {
	for _, tc := range []struct{ cpu, memory string }{
		{"two", ""},
		{"", "4GB!"},
		{"0", ""},
		{"", "-1Gi"},
		// Substrate's bounds: cpu below 1000 cores, memory an int64 byte
		// count, each quantity at most 32 characters.
		{"1000", ""},
		{"1e3", ""},
		{"999.9991", ""},
		{"", "9223372036854775808"},
		{"0.0000000000000000000000000000001", ""},
	} {
		cfg := testTemplateConfig()
		cfg.cpu, cfg.memory = tc.cpu, tc.memory
		if err := cfg.validateResources(); err == nil {
			t.Errorf("cpu=%q memory=%q: want error, got nil", tc.cpu, tc.memory)
		}
	}
	for _, tc := range []struct{ cpu, memory string }{
		{"999.999", "9223372036854775807"},
		{"500m", "8Gi"},
		{"", ""},
	} {
		cfg := testTemplateConfig()
		cfg.cpu, cfg.memory = tc.cpu, tc.memory
		if err := cfg.validateResources(); err != nil {
			t.Errorf("cpu=%q memory=%q: %v, want accepted", tc.cpu, tc.memory, err)
		}
	}
}

func TestManifestTemplateCommandRejectsEmptyResource(t *testing.T) {
	for _, flag := range []string{"--cpu=", "--memory="} {
		cmd := newManifestCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"template", "--guest-image=example.com/guest@sha256:aaaa", "--snapshots-bucket=gs://b/", flag})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "must not be empty") {
			t.Errorf("%s: got %v, want an error about the empty value", flag, err)
		}
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
		"ate.dev/api-access: \"true\"",
		"kind: WorkerPool",
		"kind: Deployment",
		"kind: Service",
		"sandboxClasses:",
		"configRef:",
		"name: gvisor-default",
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
		"wakeupProbe:",
		"snapshotConfig:",
		"storageLocation: gs://bucket/ate-env/",
		"preferredFidelity: SNAPSHOT_FIDELITY_MEMORY",
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
		"--cpu=4",
		"--memory=8Gi",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute manifest template: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "image: example.com/guest@sha256:aaaa") || !strings.Contains(out, "storageLocation: gs://bucket/prefix/") {
		t.Errorf("expected guest image and storage location in output:\n%s", out)
	}
	for _, want := range []string{"sandboxClass: gvisor", "name: cpu\n    quantity: \"4\"", "name: memory\n    quantity: 8Gi"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

const testHex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestBuildTemplateRootfsImage(t *testing.T) {
	cfg := testTemplateConfig()
	cfg.guestImage = "example.com/guest@sha256:" + testHex64
	cfg.rootfsImage = "docker.io/library/python@sha256:" + testHex64
	cfg.workspace = "/workspace"

	tmpl, err := buildTemplate(cfg)
	if err != nil {
		t.Fatalf("buildTemplate: %v", err)
	}
	if tmpl.GetMetadata().GetName() != "default-template" {
		t.Errorf("name = %q, want the configured template name unchanged", tmpl.GetMetadata().GetName())
	}
	c := tmpl.GetContainers()[0]
	if c.GetImage() != cfg.rootfsImage {
		t.Errorf("container image = %q, want the rootfs image", c.GetImage())
	}
	if len(tmpl.GetVolumes()) != 1 || tmpl.GetVolumes()[0].GetImage().GetReference() != cfg.guestImage {
		t.Errorf("volumes = %v, want the guest image volume", tmpl.GetVolumes())
	}
	if len(c.GetVolumeMounts()) != 1 || c.GetVolumeMounts()[0].GetMountPath() != apiservice.GuestMountPath {
		t.Errorf("mounts = %v", c.GetVolumeMounts())
	}
	want := []string{"/ate/ko-app/ate-env-guest", "-workspace", "/workspace"}
	if got := c.GetCommand(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("command = %v, want %v", got, want)
	}
}

func TestBuildTemplateWorkspaceOnly(t *testing.T) {
	cfg := testTemplateConfig()
	cfg.workspace = "/w"
	tmpl, err := buildTemplate(cfg)
	if err != nil {
		t.Fatalf("buildTemplate: %v", err)
	}
	c := tmpl.GetContainers()[0]
	if c.GetImage() != cfg.guestImage || len(tmpl.GetVolumes()) != 0 {
		t.Errorf("without --rootfs-image the guest stays the container image and no volume is added: %v", tmpl)
	}
	if got := c.GetCommand(); len(got) != 3 || got[0] != "/ko-app/ate-env-guest" || got[2] != "/w" {
		t.Errorf("command = %v", got)
	}
}

func TestTemplateConfigResolveImagesRootfsImage(t *testing.T) {
	cfg := testTemplateConfig()
	cfg.rootfsImage = "python:3.12"
	if err := cfg.resolveImages(); err == nil {
		t.Error("unpinned --rootfs-image accepted")
	}
	cfg.rootfsImage = "docker.io/library/python@sha256:" + testHex64
	if err := cfg.resolveImages(); err == nil {
		t.Error("short guest digest accepted together with --rootfs-image")
	}
	cfg.guestImage = "example.com/guest@sha256:" + testHex64
	if err := cfg.resolveImages(); err != nil {
		t.Errorf("pinned images rejected: %v", err)
	}
}

func TestBuildTemplateWithRuntimeLayer(t *testing.T) {
	cfg := testTemplateConfig()
	cfg.guestImage = "example.com/guest@sha256:" + testHex64
	cfg.rootfsImage = "docker.io/library/python@sha256:" + testHex64
	cfg.layers = []string{"execd=example.com/execd@sha256:" + testHex64 + "=/opt/opensandbox"}
	cfg.sidecars = []string{"/opt/opensandbox/execd --port 44772"}
	cfg.sidecarReadyz = []string{"http://127.0.0.1:44772/ready"}

	tmpl, err := buildTemplate(cfg)
	if err != nil {
		t.Fatalf("buildTemplate: %v", err)
	}
	vols := tmpl.GetVolumes()
	if len(vols) != 2 || vols[0].GetName() != "execd" || vols[0].GetImage().GetReference() != "example.com/execd@sha256:"+testHex64 || vols[1].GetName() != apiservice.GuestVolumeName {
		t.Errorf("volumes = %v, want the execd layer then the guest volume", vols)
	}
	c := tmpl.GetContainers()[0]
	if len(c.GetVolumeMounts()) != 2 || c.GetVolumeMounts()[0].GetMountPath() != "/opt/opensandbox" || c.GetVolumeMounts()[1].GetMountPath() != apiservice.GuestMountPath {
		t.Errorf("mounts = %v", c.GetVolumeMounts())
	}
	want := "/ate/ko-app/ate-env-guest -sidecar /opt/opensandbox/execd --port 44772 -sidecar-readyz http://127.0.0.1:44772/ready"
	if got := strings.Join(c.GetCommand(), " "); got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
	if c.GetImage() != cfg.rootfsImage {
		t.Errorf("image = %q", c.GetImage())
	}
}

func TestParseLayerRejects(t *testing.T) {
	cases := map[string]string{
		"missing parts":    "execd=example.com/execd@sha256:" + testHex64,
		"unpinned image":   "execd=example.com/execd:latest=/opt/execd",
		"relative mount":   "execd=example.com/execd@sha256:" + testHex64 + "=opt/execd",
		"guest mount path": "execd=example.com/execd@sha256:" + testHex64 + "=/ate",
		"reserved name":    "guest=example.com/execd@sha256:" + testHex64 + "=/opt/execd",
	}
	for name, v := range cases {
		if _, err := parseLayer(v); err == nil {
			t.Errorf("%s: %q accepted", name, v)
		}
	}
	dup := "a=example.com/x@sha256:" + testHex64 + "=/opt/a"
	if _, err := parseLayers([]string{dup, dup}); err == nil {
		t.Error("duplicate layer accepted")
	}
	cfg := testTemplateConfig()
	cfg.sidecars = []string{"execd --port 1"}
	if err := cfg.resolveImages(); err == nil {
		t.Error("relative sidecar binary accepted")
	}
	cfg = testTemplateConfig()
	cfg.sidecarReadyz = []string{"127.0.0.1:44772/ready"}
	if err := cfg.resolveImages(); err == nil {
		t.Error("sidecar-readyz without a scheme accepted")
	}
}

const relHex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestImageFlagsDefaultToReleaseImages(t *testing.T) {
	// A source build has no defaults and the flags are required.
	cmd := newManifestTemplateCommand()
	if got := cmd.Flags().Lookup("guest-image").DefValue; got != "" {
		t.Errorf("source build guest-image default = %q, want empty", got)
	}
	cfg := templateConfig{template: "t", snapshotsBucket: "gs://b/"}
	if err := cfg.resolveImages(); err == nil || !strings.Contains(err.Error(), "built from source") {
		t.Errorf("missing --guest-image should name the source-build case, got %v", err)
	}

	// A release build carries the digests; the flags default to them.
	oldAPI, oldGuest := defaultAPIImage, defaultGuestImage
	defaultAPIImage = "ghcr.io/example/ate-env-api@sha256:" + relHex64
	defaultGuestImage = "ghcr.io/example/ate-env-guest@sha256:" + relHex64
	t.Cleanup(func() { defaultAPIImage, defaultGuestImage = oldAPI, oldGuest })

	if got := newManifestTemplateCommand().Flags().Lookup("guest-image").DefValue; got != defaultGuestImage {
		t.Errorf("guest-image default = %q, want the release image", got)
	}
	if got := newManifestCommand().Flags().Lookup("api-image").DefValue; got != defaultAPIImage {
		t.Errorf("api-image default = %q, want the release image", got)
	}
	usage := newManifestCommand().Flags().Lookup("api-image").Usage
	if !strings.Contains(usage, "published with this release") {
		t.Errorf("api-image help should say the default is the release image, got %q", usage)
	}
	// The worker image stays required: it comes from the Substrate repo.
	m := manifestConfig{apiImage: defaultAPIImage}
	if err := m.resolveImages(); err == nil || !strings.Contains(err.Error(), "Substrate") {
		t.Errorf("missing --worker-image should point at the Substrate repo, got %v", err)
	}
}

func envMap(c *ateapipb.Container) (map[string]string, []string) {
	m := map[string]string{}
	var order []string
	for _, e := range c.GetEnv() {
		m[e.GetName()] = e.GetValue()
		order = append(order, e.GetName())
	}
	return m, order
}

func TestBuildTemplateEnv(t *testing.T) {
	cfg := testTemplateConfig()
	cfg.env = []string{"OH_SESSION_API_KEYS_0=s3cret", "EMPTY=", "URL=http://x/?a=b"}
	tmpl, err := buildTemplate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, order := envMap(tmpl.Containers[0])
	if strings.Join(order, ",") != "PORT,OH_SESSION_API_KEYS_0,EMPTY,URL" {
		t.Errorf("env order = %v, want PORT first, then the flags in order", order)
	}
	if got["PORT"] != "80" || got["OH_SESSION_API_KEYS_0"] != "s3cret" || got["EMPTY"] != "" || got["URL"] != "http://x/?a=b" {
		t.Errorf("env = %v", got)
	}

	// A rootfs-image template keeps the variables: the sidecars that read them
	// run there.
	cfg.rootfsImage = "docker.io/library/python@sha256:" + testHex64
	cfg.guestImage = "example.com/guest@sha256:" + testHex64
	tmpl, err = buildTemplate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := envMap(tmpl.Containers[0]); got["OH_SESSION_API_KEYS_0"] != "s3cret" {
		t.Errorf("rootfs-image template env = %v, want the --env values", got)
	}
}

func TestTemplateConfigRejectsBadEnv(t *testing.T) {
	for _, env := range [][]string{
		{"NOEQUALS"},
		{"1BAD=x"},
		{"BAD-NAME=x"},
		{"=x"},
		{"PORT=81"},
		{"A=1", "A=2"},
	} {
		cfg := testTemplateConfig()
		cfg.env = env
		if err := cfg.validateEnv(); err == nil {
			t.Errorf("env %q: want error, got nil", env)
		}
	}
}

func TestManifestTemplateCommandEnv(t *testing.T) {
	cmd := newManifestCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"template",
		"--guest-image=example.com/guest@sha256:aaaa",
		"--snapshots-bucket=gs://bucket/prefix/",
		"--env=OH_SESSION_API_KEYS_0=k",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "name: OH_SESSION_API_KEYS_0\n    value: k") {
		t.Errorf("output lacks the --env variable:\n%s", out)
	}
}

func TestManifestTemplateCommandRejectsBadEnv(t *testing.T) {
	cmd := newManifestCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"template",
		"--guest-image=example.com/guest@sha256:aaaa",
		"--snapshots-bucket=gs://bucket/prefix/",
		"--env=PORT=81",
	})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--env PORT") {
		t.Errorf("execute with --env PORT=81: want the --env error, got %v", err)
	}
}
