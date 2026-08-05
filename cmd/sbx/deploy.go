package main

import (
	"errors"
	"fmt"
	"io"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/sandbox/internal/service"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// defaultPauseImage is the digest-pinned pause image recommended by the
// Substrate ActorTemplate documentation for off-GCP clusters.
const defaultPauseImage = "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

// apiName is the name of the API service Deployment and Service.
const apiName = "sbx-api"

type deployConfig struct {
	namespace       string
	template        string
	workerPool      string
	guestImage      string
	ateomImage      string
	pauseImage      string
	snapshotsBucket string
	replicas        int32
	apiImage        string
	apiReplicas     int32
	apiPort         int32
	guestCommand    []string
	poolLabels      map[string]string
}

func newDeployCommand() *cobra.Command {
	cfg := deployConfig{}

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Generate Kubernetes manifests to deploy the system",
		Long: `Deploy generates Kubernetes manifests for everything sandboxes need on
a cluster that already runs the Agent Substrate system: the target
namespace, a WorkerPool of pre-warmed workers, the ActorTemplate that
sandboxes are created from, and the sbx-api service. It prints YAML to
stdout without touching the cluster; apply it with kubectl.

Each release publishes images:

  sbx deploy \
    --guest-image ghcr.io/agent-substrate/sandbox/sbx-guest:latest \
    --api-image   ghcr.io/agent-substrate/sandbox/sbx-api:latest \
    --ateom-image ghcr.io/agent-substrate/sandbox/ateom-gvisor:latest \
    --snapshots-bucket gs://<bucket>/substrate-sandbox/ | kubectl apply -f -

Images must be pinned by digest (repo@sha256:...); Substrate rejects
unpinned images because changing an image invalidates snapshots. To use
your own images, build and push them with ko:

  export KO_DOCKER_REPO=gcr.io/<your-project>
  sbx deploy \
    --guest-image $(ko build github.com/agent-substrate/sandbox/cmd/sbx-guest) \
    --api-image   $(ko build github.com/agent-substrate/sandbox/cmd/sbx-api) \
    --ateom-image  $(cd <substrate-checkout> && ko build ./cmd/ateom-gvisor) \
    --snapshots-bucket gs://<bucket>/substrate-sandbox/ \
    --template sandbox --namespace substrate-sandbox | kubectl apply -f -`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.resolveImages(); err != nil {
				return err
			}
			if cfg.template == "" {
				cfg.template = service.DefaultTemplate
			}
			if cfg.workerPool == "" {
				cfg.workerPool = cfg.template + "-workerpool"
			}
			cfg.poolLabels = map[string]string{"workload": cfg.template}

			return writeManifests(cmd.OutOrStdout(), buildManifests(cfg))
		},
	}

	cmd.Flags().StringVar(&cfg.namespace, "namespace", service.DefaultNamespace, "Kubernetes namespace to deploy into")
	cmd.Flags().StringVar(&cfg.template, "template", service.DefaultTemplate, "ActorTemplate name")
	cmd.Flags().StringVar(&cfg.guestImage, "guest-image", "", "digest-pinned sbx-guest image (repo@sha256:...)")
	cmd.Flags().StringVar(&cfg.ateomImage, "ateom-image", "", "digest-pinned ateom image for the worker pool, e.g. ateom-gvisor built from the Substrate repo")
	cmd.Flags().StringVar(&cfg.snapshotsBucket, "snapshots-bucket", "", "object-storage bucket (with optional prefix) for suspend snapshots, e.g. gs://bucket/prefix/")
	cmd.Flags().StringVar(&cfg.pauseImage, "pause-image", defaultPauseImage, "digest-pinned pause image for the root sandbox container")
	cmd.Flags().StringVar(&cfg.apiImage, "api-image", "", "digest-pinned sbx-api image for the API service")
	cmd.Flags().Int32Var(&cfg.apiReplicas, "api-replicas", 1, "number of API service replicas")
	cmd.Flags().Int32Var(&cfg.apiPort, "api-port", 7777, "port the sbx-api service listens on")
	cmd.Flags().StringVar(&cfg.workerPool, "workerpool", "", "WorkerPool name (defaults to <template>-workerpool)")
	cmd.Flags().Int32Var(&cfg.replicas, "replicas", 2, "number of pre-warmed worker pods")
	cmd.Flags().StringSliceVar(&cfg.guestCommand, "guest-command", []string{"/ko-app/sbx-guest"}, "guest container entrypoint")
	cmd.MarkFlagRequired("snapshots-bucket")

	return cmd
}

// resolveImages verifies that all deployment images are set, either baked
// in at release time or passed as flags.
func (c *deployConfig) resolveImages() error {
	if c.guestImage != "" && c.ateomImage != "" && c.apiImage != "" {
		return nil
	}
	return errors.New(`--guest-image, --api-image, and --ateom-image are required; use the
digest-pinned images published by the latest release (the README
quickstart records them), or build and push your own with ko:

  export KO_DOCKER_REPO=<your-registry>
  sbx deploy \
    --guest-image $(ko build github.com/agent-substrate/sandbox/cmd/sbx-guest) \
    --api-image    $(ko build github.com/agent-substrate/sandbox/cmd/sbx-api) \
    --ateom-image  $(cd <substrate-checkout> && ko build ./cmd/ateom-gvisor) \
    ...`)
}

// buildManifests returns the Kubernetes objects that make up a deployment,
// in apply order.
func buildManifests(cfg deployConfig) []any {
	return []any{
		buildNamespace(cfg),
		buildWorkerPool(cfg),
		buildActorTemplate(cfg),
		buildAPIDeployment(cfg),
		buildAPIService(cfg),
	}
}

// writeManifests writes the objects as a multi-document YAML stream.
func writeManifests(w io.Writer, objs []any) error {
	for i, obj := range objs {
		data, err := yaml.Marshal(obj)
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

func buildNamespace(cfg deployConfig) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: cfg.namespace},
	}
}

func buildWorkerPool(cfg deployConfig) *atev1alpha1.WorkerPool {
	return &atev1alpha1.WorkerPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: atev1alpha1.GroupVersion.String(),
			Kind:       "WorkerPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.workerPool,
			Namespace: cfg.namespace,
			Labels:    cfg.poolLabels,
		},
		Spec: atev1alpha1.WorkerPoolSpec{
			Replicas:   cfg.replicas,
			AteomImage: cfg.ateomImage,
		},
	}
}

func buildActorTemplate(cfg deployConfig) *atev1alpha1.ActorTemplate {
	port := "80"
	return &atev1alpha1.ActorTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: atev1alpha1.GroupVersion.String(),
			Kind:       "ActorTemplate",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.template,
			Namespace: cfg.namespace,
		},
		Spec: atev1alpha1.ActorTemplateSpec{
			PauseImage: cfg.pauseImage,
			WorkerSelector: &metav1.LabelSelector{
				MatchLabels: cfg.poolLabels,
			},
			Containers: []atev1alpha1.Container{{
				Name:    "guest",
				Image:   cfg.guestImage,
				Command: cfg.guestCommand,
				Env: []atev1alpha1.EnvVar{{
					Name:  "PORT",
					Value: &port,
				}},
				Readyz: &atev1alpha1.ContainerReadyz{
					HTTPGet: &atev1alpha1.HTTPGetAction{
						Path: "/healthz",
						Port: 80,
					},
				},
			}},
			SnapshotsConfig: atev1alpha1.SnapshotsConfig{
				Location: cfg.snapshotsBucket,
			},
		},
	}
}

// buildAPIDeployment returns the sbx-api Deployment, pointed at the
// in-cluster Substrate endpoints.
func buildAPIDeployment(cfg deployConfig) *appsv1.Deployment {
	labels := map[string]string{"app": apiName}
	replicas := cfg.apiReplicas
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
						Args:  []string{"-listen", fmt.Sprintf("0.0.0.0:%d", cfg.apiPort)},
						Ports: []corev1.ContainerPort{{ContainerPort: cfg.apiPort}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt32(cfg.apiPort),
								},
							},
						},
					}},
				},
			},
		},
	}
}

func buildAPIService(cfg deployConfig) *corev1.Service {
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
