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
		namespace:       "ate-env",
		template:        "default-env",
		workerPool:      "default-env-workerpool",
		guestImage:      "example.com/guest@sha256:aaaa",
		workerImage:     "example.com/ateom@sha256:bbbb",
		apiImage:        "example.com/api@sha256:cccc",
		snapshotsBucket: "gs://bucket/ate-env/",
		replicas:        3,
		apiReplicas:     1,
		apiPort:         7777,
		guestCommand:    []string{"/ko-app/ate-env-guest"},
		poolLabels:      map[string]string{"workload": "default-env"},
	}
}

func TestResolveImages(t *testing.T) {
	cfg := testManifestConfig()
	if err := cfg.resolveImages(); err != nil {
		t.Errorf("resolveImages with images set: %v", err)
	}
	cfg.apiImage = ""
	if err := cfg.resolveImages(); err == nil {
		t.Error("resolveImages with a missing api image: want error, got nil")
	}
	cfg = testManifestConfig()
	cfg.guestImage = ""
	if err := cfg.resolveImages(); err == nil {
		t.Error("resolveImages with a missing guest image: want error, got nil")
	}
}

func TestBuildManifests(t *testing.T) {
	cfg := testManifestConfig()
	objs := buildManifests(cfg)
	if len(objs) != 5 {
		t.Fatalf("got %d manifests, want 5", len(objs))
	}

	ns := objs[0].(*corev1.Namespace)
	if ns.Name != "ate-env" {
		t.Errorf("namespace = %q, want ate-env", ns.Name)
	}

	pool := objs[1].(*atev1alpha1.WorkerPool)
	if pool.Namespace != cfg.namespace || pool.Name != "default-env-workerpool" {
		t.Errorf("workerpool = %s/%s, want %s/default-env-workerpool", pool.Namespace, pool.Name, cfg.namespace)
	}
	if pool.Spec.Replicas != 3 || pool.Spec.WorkerImage != cfg.workerImage {
		t.Errorf("workerpool spec = %+v, want replicas 3 and worker image %q", pool.Spec, cfg.workerImage)
	}
	if pool.Labels["workload"] != "default-env" {
		t.Errorf("workerpool labels = %v, want workload=default-env", pool.Labels)
	}

	template := objs[2].(*ateapipb.ActorTemplate)
	if len(template.Containers) != 1 || template.Containers[0].Image != cfg.guestImage {
		t.Errorf("template containers = %+v, want one guest container with image %q",
			template.Containers, cfg.guestImage)
	}
	if template.GetSnapshotsConfig().GetStorageLocation() != cfg.snapshotsBucket {
		t.Errorf("snapshots location = %q, want %q", template.GetSnapshotsConfig().GetStorageLocation(), cfg.snapshotsBucket)
	}
	if got := template.GetWorkerSelector().GetMatchLabels()["workload"]; got != "default-env" {
		t.Errorf("worker selector = %v, want workload=default-env", template.WorkerSelector)
	}
	readyz := template.Containers[0].Readyz
	if readyz == nil || readyz.GetHttpGet() == nil || readyz.GetHttpGet().Path != "/readyz" {
		t.Errorf("readyz = %+v, want HTTP GET /readyz", readyz)
	}

	deployment := objs[3].(*appsv1.Deployment)
	containers := deployment.Spec.Template.Spec.Containers
	if len(containers) != 1 || containers[0].Image != cfg.apiImage {
		t.Errorf("api containers = %+v, want one container with image %q", containers, cfg.apiImage)
	}
	if *deployment.Spec.Replicas != 1 {
		t.Errorf("api replicas = %d, want 1", *deployment.Spec.Replicas)
	}

	service := objs[4].(*corev1.Service)
	if service.Spec.Selector["app"] != apiName {
		t.Errorf("api service selector = %v, want app=%s", service.Spec.Selector, apiName)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 7777 || service.Spec.Ports[0].TargetPort.IntValue() != 7777 {
		t.Errorf("api service ports = %+v, want 7777 -> 7777", service.Spec.Ports)
	}
}

func TestBuildManifestsCustomAPIPort(t *testing.T) {
	cfg := testManifestConfig()
	cfg.apiPort = 9999
	objs := buildManifests(cfg)

	deployment := objs[3].(*appsv1.Deployment)
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

	service := objs[4].(*corev1.Service)
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

	if got := strings.Count(out, "\n---\n"); got != 4 {
		t.Errorf("got %d document separators, want 4:\n%s", got, out)
	}
	for _, want := range []string{
		"kind: Namespace",
		"kind: WorkerPool",
		"name: default-env",
		"kind: Deployment",
		"kind: Service",
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
