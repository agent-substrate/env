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
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/agent-substrate/env/internal/apiservice"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// apiName is the name of the API service Deployment and Service.
const apiName = "ate-env-api"

// manifestConfig holds configuration for generating Kubernetes deployment manifests.
type manifestConfig struct {
	namespace   string
	template    string
	workerPool  string
	workerImage string
	replicas    int32
	apiImage    string
	apiReplicas int32
	apiPort     int32
}

func (c *manifestConfig) resolveImages() error {
	if c.workerImage == "" || c.apiImage == "" {
		return errors.New(`--api-image and --worker-image (or --ateom-image) are required; use the
digest-pinned images published by the latest release (the README
quickstart records them), or build and push your own.`)
	}
	return nil
}

// templateConfig holds configuration for generating a Substrate ActorTemplate manifest.
type templateConfig struct {
	template        string
	atespace        string
	guestImage      string
	guestCommand    []string
	snapshotsBucket string
}

func (c *templateConfig) resolveImages() error {
	if c.guestImage == "" {
		return errors.New("--guest-image is required; use the digest-pinned ate-env-guest image")
	}
	if c.snapshotsBucket == "" {
		return errors.New("--snapshots-bucket is required; use an object-storage bucket (e.g. gs://bucket/prefix/)")
	}
	return nil
}

func newManifestCommand() *cobra.Command {
	mCfg := manifestConfig{}

	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Generate Kubernetes manifests to deploy the system",
		Long: `Manifest generates Kubernetes manifests for everything environments need on
a cluster that already runs the Agent Substrate system: the target
namespace, a WorkerPool of pre-warmed workers, and the ate-env-api service.
It prints YAML to stdout without touching the cluster; apply it with kubectl.

To generate the Substrate ActorTemplate manifest (for kubectl-ate), use
the "template" subcommand: ate-env manifest template`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if mCfg.template == "" {
				mCfg.template = apiservice.DefaultTemplate
			}
			if mCfg.workerPool == "" {
				mCfg.workerPool = mCfg.template + "-workerpool"
			}
			if err := mCfg.resolveImages(); err != nil {
				return err
			}
			return writeManifests(cmd.OutOrStdout(), buildManifests(mCfg))
		},
	}

	cmd.Flags().StringVar(&mCfg.namespace, "namespace", apiservice.DefaultNamespace, "Kubernetes namespace to deploy into")
	cmd.Flags().StringVar(&mCfg.template, "template", apiservice.DefaultTemplate, "ActorTemplate name")
	cmd.Flags().StringVar(&mCfg.workerImage, "worker-image", "", "digest-pinned worker image for the worker pool, e.g. ateom-gvisor built from the Substrate repo")
	cmd.Flags().StringVar(&mCfg.workerImage, "ateom-image", "", "alias for --worker-image")
	cmd.Flags().StringVar(&mCfg.apiImage, "api-image", "", "digest-pinned ate-env-api image for the API service")
	cmd.Flags().Int32Var(&mCfg.apiReplicas, "api-replicas", 1, "number of API service replicas")
	cmd.Flags().Int32Var(&mCfg.apiPort, "api-port", 7777, "port the ate-env-api service listens on")
	cmd.Flags().StringVar(&mCfg.workerPool, "workerpool", "", "WorkerPool name (defaults to <template>-workerpool)")
	cmd.Flags().Int32Var(&mCfg.replicas, "replicas", 5, "number of pre-warmed worker pods")

	cmd.AddCommand(newManifestTemplateCommand())

	return cmd
}

func newManifestTemplateCommand() *cobra.Command {
	tCfg := templateConfig{}

	cmd := &cobra.Command{
		Use:   "template",
		Short: "Generate Substrate ActorTemplate manifest",
		Long: `Generate Substrate ActorTemplate manifest to register with kubectl-ate.
It prints YAML to stdout without touching the cluster.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if tCfg.template == "" {
				tCfg.template = apiservice.DefaultTemplate
			}
			if err := tCfg.resolveImages(); err != nil {
				return err
			}
			return writeActorTemplate(cmd.OutOrStdout(), buildActorTemplate(tCfg))
		},
	}

	cmd.Flags().StringVar(&tCfg.template, "template", apiservice.DefaultTemplate, "ActorTemplate name")
	cmd.Flags().StringVar(&tCfg.atespace, "atespace", apiservice.DefaultAtespace, "Substrate atespace for the ActorTemplate")
	cmd.Flags().StringVar(&tCfg.guestImage, "guest-image", "", "digest-pinned ate-env-guest image (repo@sha256:...)")
	cmd.Flags().StringSliceVar(&tCfg.guestCommand, "guest-command", []string{"/ko-app/ate-env-guest"}, "guest container entrypoint")
	cmd.Flags().StringVar(&tCfg.snapshotsBucket, "snapshots-bucket", "", "object-storage bucket (with optional prefix) for actor snapshots, e.g. gs://bucket/prefix/")

	return cmd
}

// buildManifests returns the Kubernetes objects that make up a deployment,
// in apply order.
func buildManifests(cfg manifestConfig) []any {
	return []any{
		buildNamespace(cfg),
		buildWorkerPool(cfg),
		buildAPIDeployment(cfg),
		buildAPIService(cfg),
	}
}

// writeActorTemplate writes a single Substrate ActorTemplate as protojson-shaped YAML.
func writeActorTemplate(w io.Writer, tmpl *ateapipb.ActorTemplate) error {
	opts := protojson.MarshalOptions{
		UseProtoNames: false,
	}
	jsonBytes, err := opts.Marshal(tmpl)
	if err != nil {
		return fmt.Errorf("encoding actor template json: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return fmt.Errorf("decoding actor template json: %w", err)
	}
	delete(m, "status")
	prune(m)
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding actor template yaml: %w", err)
	}
	_, err = w.Write(data)
	return err
}

// writeManifests writes the objects as a multi-document YAML stream.
func writeManifests(w io.Writer, objs []any) error {
	for i, obj := range objs {
		var jsonBytes []byte
		var err error
		if m, ok := obj.(proto.Message); ok {
			jsonBytes, err = protojson.Marshal(m)
		} else {
			jsonBytes, err = json.Marshal(obj)
		}
		if err != nil {
			return fmt.Errorf("encoding json: %w", err)
		}
		var m map[string]any
		if err := json.Unmarshal(jsonBytes, &m); err != nil {
			return fmt.Errorf("decoding json: %w", err)
		}
		delete(m, "status")
		prune(m)
		data, err := yaml.Marshal(m)
		if err != nil {
			return fmt.Errorf("encoding manifest: %w", err)
		}
		if i > 0 {
			if _, err := io.WriteString(w, "---\n"); err != nil {
				return err
			}
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func prune(v any) any {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			cleaned := prune(child)
			if cleaned == nil {
				delete(val, k)
				continue
			}
			if m, ok := cleaned.(map[string]any); ok && len(m) == 0 {
				delete(val, k)
				continue
			}
			val[k] = cleaned
		}
		if len(val) == 0 {
			return nil
		}
		return val
	case []any:
		for i, elem := range val {
			val[i] = prune(elem)
		}
		return val
	default:
		return val
	}
}

func buildNamespace(cfg manifestConfig) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: cfg.namespace},
	}
}

func buildWorkerPool(cfg manifestConfig) *atev1alpha1.WorkerPool {
	return &atev1alpha1.WorkerPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: atev1alpha1.GroupVersion.String(),
			Kind:       "WorkerPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.workerPool,
			Namespace: cfg.namespace,
		},
		Spec: atev1alpha1.WorkerPoolSpec{
			Replicas:    cfg.replicas,
			WorkerImage: cfg.workerImage,
		},
	}
}

func buildActorTemplate(cfg templateConfig) *ateapipb.ActorTemplate {
	atespace := cfg.atespace
	if atespace == "" {
		atespace = apiservice.DefaultAtespace
	}
	return &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{
			Name:     cfg.template,
			Atespace: atespace,
		},
		Containers: []*ateapipb.Container{{
			Name:    "guest",
			Image:   cfg.guestImage,
			Command: cfg.guestCommand,
			Env: []*ateapipb.EnvVar{{
				Name:  "PORT",
				Value: "80",
			}},
			Readyz: &ateapipb.ContainerReadyz{
				HttpGet: &ateapipb.HTTPGetAction{
					Path: "/readyz",
					Port: 80,
				},
			},
		}},
		SnapshotsConfig: &ateapipb.SnapshotsConfig{
			StorageLocation: cfg.snapshotsBucket,
		},
		SandboxConfig: &ateapipb.SandboxConfig{
			SandboxClass: ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR,
			ConfigName:   "gvisor-default",
		},
	}
}

// buildAPIDeployment returns the ate-env-api Deployment, pointed at the
// in-cluster Substrate endpoints.
func buildAPIDeployment(cfg manifestConfig) *appsv1.Deployment {
	labels := map[string]string{"app": apiName}
	replicas := cfg.apiReplicas
	tokenExpiration := int64(7200)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiName,
			Namespace: cfg.namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  apiName,
						Image: cfg.apiImage,
						// ateapi/atenet default to the in-cluster
						// Substrate service addresses.
						Args: []string{
							"-listen", fmt.Sprintf("0.0.0.0:%d", cfg.apiPort),
							"-ateapi-token-file=/var/run/secrets/ateapi/token",
						},
						Ports: []corev1.ContainerPort{{ContainerPort: cfg.apiPort}},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "ateapi-token",
							MountPath: "/var/run/secrets/ateapi",
							ReadOnly:  true,
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt32(cfg.apiPort),
								},
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "ateapi-token",
						VolumeSource: corev1.VolumeSource{
							Projected: &corev1.ProjectedVolumeSource{
								Sources: []corev1.VolumeProjection{{
									ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
										Audience:          "api.ate-system.svc",
										ExpirationSeconds: &tokenExpiration,
										Path:              "token",
									},
								}},
							},
						},
					}},
				},
			},
		},
	}
}

func buildAPIService(cfg manifestConfig) *corev1.Service {
	labels := map[string]string{"app": apiName}
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiName,
			Namespace: cfg.namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Port:       cfg.apiPort,
				TargetPort: intstr.FromInt32(cfg.apiPort),
			}},
		},
	}
}
