// Command ate-env is a CLI for the environment service. Environment commands go
// through the ate-env-api service using the env SDK; manifest generates
// Kubernetes manifests for setting up the system on a cluster.
//
// By default, ate-env automatically connects to the ate-env-api service running in
// the active Kubernetes context via port-forwarding. A specific context can be
// selected with --context, or a direct API endpoint can be specified with --api
// or the SUBSTRATE_ENV_API environment variable.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agent-substrate/env/env"
	"github.com/agent-substrate/env/internal/service"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"github.com/spf13/cobra"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	var (
		endpoint    string
		kubeContext string
		kubeconfig  string
		kubeNS      string
		atespace    string
		client      *env.Client
		stopForward func()
	)

	root := &cobra.Command{
		Use:           "ate-env",
		Short:         "Manage environments on Agent Substrate",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Name() == "manifest" || cmd.Name() == "help" || cmd.Name() == "completion" {
				return nil
			}
			targetEndpoint := endpoint
			if targetEndpoint == "" {
				fw, err := startPortForward(cmd.Context(), kubeconfig, kubeContext, kubeNS, 7777)
				if err != nil {
					return fmt.Errorf("connecting to cluster (use --api to specify endpoint directly): %w", err)
				}
				targetEndpoint = fw.endpoint
				stopForward = fw.stop
			}
			var err error
			client, err = env.NewClient(env.ClientOptions{
				Endpoint: targetEndpoint,
			})
			return err
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			if client != nil {
				client.Close()
			}
			if stopForward != nil {
				stopForward()
			}
		},
	}
	root.PersistentFlags().StringVar(&endpoint, "api", envOr("SUBSTRATE_ENV_API", ""), "address of the ate-env-api service (e.g. localhost:7777)")
	root.PersistentFlags().StringVar(&kubeContext, "context", "", "Kubernetes context to use")
	root.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	root.PersistentFlags().StringVar(&kubeNS, "kube-namespace", service.DefaultNamespace, "Kubernetes namespace where ate-env-api is deployed")
	root.PersistentFlags().StringVar(&atespace, "atespace", "default", "Substrate atespace")

	root.AddCommand(newManifestCommand())

	var (
		createTemplate  string
		createNamespace string
	)
	createCmd := &cobra.Command{
		Use:   "create <id>",
		Short: "Create and start an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &ateenvv1.CreateEnvironmentRequest{
				Id:       args[0],
				Atespace: atespace,
			}
			if createTemplate != "" || createNamespace != "" {
				req.Template = &ateenvv1.Template{
					Name:      createTemplate,
					Namespace: createNamespace,
				}
			}
			_, err := client.Create(cmd.Context(), req)
			return err
		},
	}
	createCmd.Flags().StringVar(&createTemplate, "template", "", "ActorTemplate name (defaults to server default)")
	createCmd.Flags().StringVar(&createNamespace, "namespace", "", "Kubernetes namespace of the ActorTemplate (defaults to server default)")
	root.AddCommand(createCmd)

	root.AddCommand(&cobra.Command{
		Use:   "suspend <id>",
		Short: "Suspend an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Suspend(cmd.Context(), atespace, args[0])
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "delete <id>",
		Short: "Delete an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Delete(cmd.Context(), atespace, args[0])
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "read <id> <path>",
		Short: "Print an environment file to stdout",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := client.Env(atespace, args[0]).ReadFile(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(os.Stdout, rc)
			return err
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "write <id> <path>",
		Short: "Write stdin to an environment file",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Env(atespace, args[0]).WriteFile(cmd.Context(), args[1], os.Stdin, 0o644)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:     "shell <id> <cmdline>",
		Aliases: []string{"cmd"},
		Short:   "Run a shell command line in the environment",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := client.Env(atespace, args[0]).Shell(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			if out := strings.Trim(res.Stdout, "\r\n"); out != "" {
				fmt.Println(out)
			}
			if errOut := strings.Trim(res.Stderr, "\r\n"); errOut != "" {
				fmt.Fprintln(os.Stderr, errOut)
			}
			if res.ExitCode != 0 {
				os.Exit(res.ExitCode)
			}
			return nil
		},
	})

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ate-env:", err)
		os.Exit(1)
	}
}
