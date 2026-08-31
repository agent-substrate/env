package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/agent-substrate/env/internal/apiservice"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// apiName is the name of the API service Deployment and Service.
const apiName = "ate-env-api"

type manifestConfig struct {
	namespace       string
	template        string
	workerPool      string
	guestImage      string
	ateomImage      string
	snapshotsBucket string
	replicas        int32
	apiImage        string
	apiReplicas     int32
	apiPort         int32
	idleTTL         string
	guestCommand    []string
	poolLabels      map[string]string
}

func newManifestCommand() *cobra.Command {
	cfg := manifestConfig{}

	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Generate Kubernetes manifests to deploy the system",
		Long: `Manifest generates Kubernetes manifests for everything environments need on
a cluster that already runs the Agent Substrate system: the target
namespace, a WorkerPool of pre-warmed workers, the ActorTemplate that
environments are created from, and the ate-env-api service. It prints YAML to
stdout without touching the cluster; apply it with kubectl.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.resolveImages(); err != nil {
				return err
			}
			if cfg.template == "" {
				cfg.template = apiservice.DefaultTemplate
			}
			if cfg.workerPool == "" {
				cfg.workerPool = cfg.template + "-workerpool"
			}
			cfg.poolLabels = map[string]string{"workload": cfg.template}

			return writeManifests(cmd.OutOrStdout(), buildManifests(cfg))
		},
	}

	cmd.Flags().StringVar(&cfg.namespace, "namespace", apiservice.DefaultNamespace, "Kubernetes namespace to deploy into")
	cmd.Flags().StringVar(&cfg.template, "template", apiservice.DefaultTemplate, "ActorTemplate name")
	cmd.Flags().StringVar(&cfg.guestImage, "guest-image", "", "digest-pinned ate-env-guest image (repo@sha256:...)")
	cmd.Flags().StringVar(&cfg.ateomImage, "ateom-image", "", "digest-pinned ateom image for the worker pool, e.g. ateom-gvisor built from the Substrate repo")
	cmd.Flags().StringVar(&cfg.snapshotsBucket, "snapshots-bucket", "", "object-storage bucket (with optional prefix) for actor snapshots, e.g. gs://bucket/prefix/")
	cmd.Flags().StringVar(&cfg.apiImage, "api-image", "", "digest-pinned ate-env-api image for the API service")
	cmd.Flags().Int32Var(&cfg.apiReplicas, "api-replicas", 1, "number of API service replicas")
	cmd.Flags().Int32Var(&cfg.apiPort, "api-port", 7777, "port the ate-env-api service listens on")
	cmd.Flags().StringVar(&cfg.idleTTL, "idle-ttl", "", "suspend environments after this much inactivity, e.g. 90s or 5m (defaults to the ate-env-api built-in; 0 disables)")
	cmd.Flags().StringVar(&cfg.workerPool, "workerpool", "", "WorkerPool name (defaults to <template>-workerpool)")
	cmd.Flags().Int32Var(&cfg.replicas, "replicas", 5, "number of pre-warmed worker pods")
	cmd.Flags().StringSliceVar(&cfg.guestCommand, "guest-command", []string{"/ko-app/ate-env-guest"}, "guest container entrypoint")
	cmd.MarkFlagRequired("snapshots-bucket")

	return cmd
}

// resolveImages verifies that all deployment images are set, either baked
// in at release time or passed as flags.
func (c *manifestConfig) resolveImages() error {
	if c.guestImage != "" && c.ateomImage != "" && c.apiImage != "" {
		return nil
	}
	return errors.New(`--guest-image, --api-image, and --ateom-image are required; use the
digest-pinned images published by the latest release (the README
quickstart records them), or build and push your own.`)
}

// buildManifests returns the Kubernetes objects that make up a deployment,
// in apply order.
func buildManifests(cfg manifestConfig) []any {
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
		jsonBytes, err := json.Marshal(obj)
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
			Labels:    cfg.poolLabels,
		},
		Spec: atev1alpha1.WorkerPoolSpec{
			Replicas:   cfg.replicas,
			AteomImage: cfg.ateomImage,
		},
	}
}

func buildActorTemplate(cfg manifestConfig) *atev1alpha1.ActorTemplate {
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
			WorkerSelector: &metav1.LabelSelector{
				MatchLabels: cfg.poolLabels,
			},
			Containers: []atev1alpha1.Container{{
				Name:    "guest",
				Image:   cfg.guestImage,
				Command: cfg.guestCommand,
				Env: []atev1alpha1.EnvVar{{
					Name:  "PORT",
					Value: port,
				}},
				Readyz: &atev1alpha1.ContainerReadyz{
					HTTPGet: &atev1alpha1.HTTPGetAction{
						Path: "/readyz",
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

// buildAPIDeployment returns the ate-env-api Deployment, pointed at the
// in-cluster Substrate endpoints.
func buildAPIDeployment(cfg manifestConfig) *appsv1.Deployment {
	labels := map[string]string{"app": apiName}
	replicas := cfg.apiReplicas
	tokenExpiration := int64(7200)
	args := []string{
		"-listen", fmt.Sprintf("0.0.0.0:%d", cfg.apiPort),
		"-ateapi-token-file=/var/run/secrets/ateapi/token",
	}
	if cfg.idleTTL != "" {
		args = append(args, "-idle-ttl="+cfg.idleTTL)
	}
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
						Args: args,
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
