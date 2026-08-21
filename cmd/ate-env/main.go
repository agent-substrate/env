// Command ate-env is a CLI for the environment service. Environment commands go
// through the ate-env-api service using the env SDK; deploy generates
// Kubernetes manifests for setting up the system on a cluster.
//
// The API endpoint can be set with the --api flag or the
// SUBSTRATE_ENV_API environment variable.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agent-substrate/env/env"
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
		endpoint string
		atespace string
		client   *env.Client
	)

	root := &cobra.Command{
		Use:           "ate-env",
		Short:         "Manage environments on Agent Substrate",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Name() == "deploy" {
				return nil
			}
			var err error
			client, err = env.NewClient(env.ClientOptions{
				Endpoint: endpoint,
			})
			return err
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			if client != nil {
				client.Close()
			}
		},
	}
	root.PersistentFlags().StringVar(&endpoint, "api", envOr("SUBSTRATE_ENV_API", "127.0.0.1:7777"), "address of the ate-env-api service (e.g. localhost:7777)")
	root.PersistentFlags().StringVar(&atespace, "atespace", "default", "Substrate atespace")

	root.AddCommand(newDeployCommand())

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
	createCmd.Flags().StringVar(&createTemplate, "template", "default-env", "ActorTemplate name")
	createCmd.Flags().StringVar(&createNamespace, "namespace", "ate-env", "Kubernetes namespace of the ActorTemplate")
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
