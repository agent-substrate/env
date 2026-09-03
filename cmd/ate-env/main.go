// Command ate-env is a CLI for the environment service. Environment commands go
// through the ate-env-api service using the env SDK; manifest generates
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

	"github.com/agent-substrate/env/clients/go/env"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"github.com/spf13/cobra"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newGuestCommand(id string) *cobra.Command {
	var (
		endpoint string
		atespace string
		client   *env.Client
	)

	guestCmd := &cobra.Command{
		Use:   id,
		Short: fmt.Sprintf("Manage environment %s", id),
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
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
	guestCmd.PersistentFlags().StringVar(&endpoint, "api", envOr("SUBSTRATE_ENV_API", "127.0.0.1:7777"), "address of the ate-env-api service (e.g. localhost:7777)")
	guestCmd.PersistentFlags().StringVar(&atespace, "atespace", "default", "Substrate atespace")

	guestCmd.AddCommand(&cobra.Command{
		Use:   "read <path>",
		Short: "Print an environment file to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := client.Env(atespace, id).ReadFile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(os.Stdout, rc)
			return err
		},
	})

	guestCmd.AddCommand(&cobra.Command{
		Use:   "write <path>",
		Short: "Write stdin to an environment file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Env(atespace, id).WriteFile(cmd.Context(), args[0], os.Stdin, 0o644)
		},
	})

	guestCmd.AddCommand(&cobra.Command{
		Use:     "shell <cmdline>",
		Aliases: []string{"cmd"},
		Short:   "Run a shell command line in the environment",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := client.Env(atespace, id).Shell(cmd.Context(), strings.Join(args, " "))
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

	return guestCmd
}

func newCreateCommand() *cobra.Command {
	var (
		endpoint               string
		atespace               string
		createTemplate         string
		createTemplateAtespace string
	)
	cmd := &cobra.Command{
		Use:   "create <id>",
		Short: "Create and start an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := env.NewClient(env.ClientOptions{
				Endpoint: endpoint,
			})
			if err != nil {
				return err
			}
			defer client.Close()

			req := &ateenvv1.CreateEnvironmentRequest{
				Id:       args[0],
				Atespace: atespace,
			}
			if createTemplate != "" || createTemplateAtespace != "" {
				tmplAtespace := createTemplateAtespace
				if tmplAtespace == "" {
					tmplAtespace = atespace
				}
				req.Template = &ateenvv1.Template{
					Name:     createTemplate,
					Atespace: tmplAtespace,
				}
			}
			_, err = client.Create(cmd.Context(), req)
			return err
		},
	}
	cmd.Flags().StringVar(&endpoint, "api", envOr("SUBSTRATE_ENV_API", "127.0.0.1:7777"), "address of the ate-env-api service (e.g. localhost:7777)")
	cmd.Flags().StringVar(&atespace, "atespace", "default", "Substrate atespace")
	cmd.Flags().StringVar(&createTemplate, "template", "", "ActorTemplate name (defaults to server default)")
	cmd.Flags().StringVar(&createTemplateAtespace, "template-atespace", "", "Substrate atespace of the ActorTemplate (defaults to environment atespace)")
	return cmd
}

func newSuspendCommand() *cobra.Command {
	var (
		endpoint string
		atespace string
	)
	cmd := &cobra.Command{
		Use:   "suspend <id>",
		Short: "Suspend an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := env.NewClient(env.ClientOptions{
				Endpoint: endpoint,
			})
			if err != nil {
				return err
			}
			defer client.Close()

			return client.Suspend(cmd.Context(), atespace, args[0])
		},
	}
	cmd.Flags().StringVar(&endpoint, "api", envOr("SUBSTRATE_ENV_API", "127.0.0.1:7777"), "address of the ate-env-api service (e.g. localhost:7777)")
	cmd.Flags().StringVar(&atespace, "atespace", "default", "Substrate atespace")
	return cmd
}

func newDeleteCommand() *cobra.Command {
	var (
		endpoint string
		atespace string
	)
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := env.NewClient(env.ClientOptions{
				Endpoint: endpoint,
			})
			if err != nil {
				return err
			}
			defer client.Close()

			return client.Delete(cmd.Context(), atespace, args[0])
		},
	}
	cmd.Flags().StringVar(&endpoint, "api", envOr("SUBSTRATE_ENV_API", "127.0.0.1:7777"), "address of the ate-env-api service (e.g. localhost:7777)")
	cmd.Flags().StringVar(&atespace, "atespace", "default", "Substrate atespace")
	return cmd
}

func newRootCommand(args []string) *cobra.Command {
	root := &cobra.Command{
		Use:   "ate-env",
		Short: "Manage environments on Agent Substrate",
		Long: `Manage environments on Agent Substrate.

Common environment commands:
  ate-env <id> read <path>       Print an environment file to stdout
  ate-env <id> write <path>      Write stdin to an environment file
  ate-env <id> shell <cmdline>   Run a shell command line in the environment`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newManifestCommand())
	root.AddCommand(newCreateCommand())
	root.AddCommand(newSuspendCommand())
	root.AddCommand(newDeleteCommand())

	if len(args) > 0 {
		cmdName := args[0]
		if _, _, err := root.Find([]string{cmdName}); err != nil {
			root.AddCommand(newGuestCommand(cmdName))
		}
	}

	return root
}

func main() {
	root := newRootCommand(os.Args[1:])
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ate-env:", err)
		os.Exit(1)
	}
}
