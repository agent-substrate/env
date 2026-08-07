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
	"text/tabwriter"

	"github.com/agent-substrate/env/env"
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
	root.PersistentFlags().StringVar(&endpoint, "api", envOr("SUBSTRATE_ENV_API", "http://127.0.0.1:7777"), "base URL of the ate-env-api service")

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
			opts := []env.CreateOption{}
			if createTemplate != "" {
				opts = append(opts, env.WithTemplate(createTemplate))
			}
			if createNamespace != "" {
				opts = append(opts, env.WithNamespace(createNamespace))
			}
			_, err := client.Create(cmd.Context(), args[0], opts...)
			return err
		},
	}
	createCmd.Flags().StringVar(&createTemplate, "template", "default-env", "ActorTemplate name")
	createCmd.Flags().StringVar(&createNamespace, "namespace", "ate-env", "Kubernetes namespace of the ActorTemplate")
	root.AddCommand(createCmd)

	// Top-level legacy commands (ate-env <command> <id> ...)
	root.AddCommand(&cobra.Command{
		Use:   "suspend <id>",
		Short: "Snapshot to external storage and free the worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Env(args[0]).Suspend(cmd.Context())
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "resume <id>",
		Short: "Resume from the latest snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Env(args[0]).Resume(cmd.Context())
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "delete <id>",
		Short: "Delete an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Env(args[0]).Delete(cmd.Context())
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "cmd <id> <cmdline>",
		Short: "Run a shell command line in the environment",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := client.Env(args[0]).Cmd(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			if out := strings.Trim(res.Stdout, "\r\n"); out != "" {
				fmt.Println(out)
			}
			if errOut := strings.Trim(res.Stderr, "\r\n"); errOut != "" {
				fmt.Fprintln(os.Stderr, errOut)
			}
			if res.TimedOut {
				fmt.Fprintln(os.Stderr, "ate-env: command timed out")
			}
			if res.ExitCode != 0 {
				os.Exit(res.ExitCode)
			}
			return nil
		},
	})

	fsCmd := &cobra.Command{
		Use:   "fs",
		Short: "Operate on files and directories in an environment",
	}
	fsCmd.AddCommand(&cobra.Command{
		Use:   "read <id> <path>",
		Short: "Print an environment file to stdout",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := client.Env(args[0]).ReadFile(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(os.Stdout, rc)
			return err
		},
	})
	fsCmd.AddCommand(&cobra.Command{
		Use:   "write <id> <path>",
		Short: "Write stdin to an environment file",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Env(args[0]).WriteFile(cmd.Context(), args[1], os.Stdin, 0o644)
		},
	})
	fsCmd.AddCommand(&cobra.Command{
		Use:   "ls <id> <path>",
		Short: "List an environment directory",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := client.Env(args[0]).ListDir(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", e.ModeString, e.Size, e.ModTime.Format("2006-01-02 15:04"), e.Name)
			}
			return tw.Flush()
		},
	})
	fsCmd.AddCommand(&cobra.Command{
		Use:   "stat <id> <path>",
		Short: "Stat an environment path",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := client.Env(args[0]).Stat(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			fmt.Printf("path:  %s\nmode:  %s\nsize:  %d\nmtime: %s\n", e.Path, e.ModeString, e.Size, e.ModTime)
			return nil
		},
	})
	fsCmd.AddCommand(&cobra.Command{
		Use:   "rm <id> <path>",
		Short: "Delete a file or directory in the environment",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Env(args[0]).Remove(cmd.Context(), args[1])
		},
	})
	fsCmd.AddCommand(&cobra.Command{
		Use:   "mkdir <id> <path>",
		Short: "Create a directory in the environment",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Env(args[0]).Mkdir(cmd.Context(), args[1], 0o755)
		},
	})
	root.AddCommand(fsCmd)

	// Dynamically register environment instance subcommands (ate-env <id> ...)
	cmd, remainingArgs, _ := root.Find(os.Args[1:])
	if cmd == root && len(remainingArgs) > 0 {
		id := remainingArgs[0]
		if !strings.HasPrefix(id, "-") {
			root.AddCommand(newEnvCommand(id, &client))
		}
	}

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ate-env:", err)
		os.Exit(1)
	}
}

func newEnvCommand(id string, client **env.Client) *cobra.Command {
	sbCmd := &cobra.Command{
		Use:   id,
		Short: fmt.Sprintf("Operate on environment %s", id),
	}

	sbCmd.AddCommand(&cobra.Command{
		Use:   "cmd <cmdline>",
		Short: "Run a shell command line in the environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := (*client).Env(id).Cmd(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if out := strings.Trim(res.Stdout, "\r\n"); out != "" {
				fmt.Println(out)
			}
			if errOut := strings.Trim(res.Stderr, "\r\n"); errOut != "" {
				fmt.Fprintln(os.Stderr, errOut)
			}
			if res.TimedOut {
				fmt.Fprintln(os.Stderr, "ate-env: command timed out")
			}
			if res.ExitCode != 0 {
				os.Exit(res.ExitCode)
			}
			return nil
		},
	})

	fsCmd := &cobra.Command{
		Use:   "fs",
		Short: "Operate on files and directories in the environment",
	}

	fsCmd.AddCommand(&cobra.Command{
		Use:   "read <path>",
		Short: "Print an environment file to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := (*client).Env(id).ReadFile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(os.Stdout, rc)
			return err
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "write <path>",
		Short: "Write stdin to an environment file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return (*client).Env(id).WriteFile(cmd.Context(), args[0], os.Stdin, 0o644)
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "ls <path>",
		Short: "List an environment directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := (*client).Env(id).ListDir(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", e.ModeString, e.Size, e.ModTime.Format("2006-01-02 15:04"), e.Name)
			}
			return tw.Flush()
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "stat <path>",
		Short: "Stat an environment path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := (*client).Env(id).Stat(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("path:  %s\nmode:  %s\nsize:  %d\nmtime: %s\n", e.Path, e.ModeString, e.Size, e.ModTime)
			return nil
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "rm <path>",
		Short: "Delete a file or directory in the environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return (*client).Env(id).Remove(cmd.Context(), args[0])
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "mkdir <path>",
		Short: "Create a directory in the environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return (*client).Env(id).Mkdir(cmd.Context(), args[0], 0o755)
		},
	})

	sbCmd.AddCommand(fsCmd)
	return sbCmd
}
