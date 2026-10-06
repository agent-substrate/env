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
	"time"

	"github.com/agent-substrate/env/clients/go"
	"github.com/agent-substrate/env/internal/apiservice"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
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
	guestCmd.PersistentFlags().StringVar(&atespace, "atespace", apiservice.DefaultAtespace, "Substrate atespace")

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

	var (
		shellStdin   bool
		shellTimeout time.Duration
	)
	shellCmd := &cobra.Command{
		Use:     "shell <cmdline>",
		Aliases: []string{"cmd"},
		Short:   "Run a shell command line in the environment",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := env.ShellRequest{Command: strings.Join(args, " "), Timeout: shellTimeout}
			if shellStdin {
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("reading stdin: %w", err)
				}
				req.Stdin = data
			}
			res, err := client.Env(atespace, id).Run(cmd.Context(), req)
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
	}
	shellCmd.Flags().BoolVarP(&shellStdin, "stdin", "i", false, "feed this process's stdin to the command")
	shellCmd.Flags().DurationVar(&shellTimeout, "timeout", 0, "kill the command after this duration (default: server default)")
	guestCmd.AddCommand(shellCmd)

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

			req := &ateenvv1alpha.CreateEnvironmentRequest{
				Id:       args[0],
				Atespace: atespace,
			}
			if createTemplate != "" || createTemplateAtespace != "" {
				tmplAtespace := createTemplateAtespace
				if tmplAtespace == "" {
					tmplAtespace = atespace
				}
				req.Template = &ateenvv1alpha.Template{
					Name:     createTemplate,
					Atespace: tmplAtespace,
				}
			}
			_, err = client.Create(cmd.Context(), req)
			return err
		},
	}
	cmd.Flags().StringVar(&endpoint, "api", envOr("SUBSTRATE_ENV_API", "127.0.0.1:7777"), "address of the ate-env-api service (e.g. localhost:7777)")
	cmd.Flags().StringVar(&atespace, "atespace", apiservice.DefaultAtespace, "Substrate atespace")
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
	cmd.Flags().StringVar(&atespace, "atespace", apiservice.DefaultAtespace, "Substrate atespace")
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
	cmd.Flags().StringVar(&atespace, "atespace", apiservice.DefaultAtespace, "Substrate atespace")
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
