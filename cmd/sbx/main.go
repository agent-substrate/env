// Command sbx is a CLI for the sandbox service. Sandbox commands go
// through the sbx-api service using the sandbox SDK; deploy generates
// Kubernetes manifests for setting up the system on a cluster.
//
// The API endpoint can be set with the --api flag or the
// SUBSTRATE_SANDBOX_API environment variable.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/agent-substrate/sandbox/sandbox"
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
		client   *sandbox.Client
	)

	root := &cobra.Command{
		Use:           "sbx",
		Short:         "Manage sandboxes on Agent Substrate",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// deploy talks to the Kubernetes API, not the sandbox API.
			if cmd.Name() == "deploy" {
				return nil
			}
			var err error
			client, err = sandbox.NewClient(sandbox.ClientOptions{
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
	root.PersistentFlags().StringVar(&endpoint, "api", envOr("SUBSTRATE_SANDBOX_API", "http://127.0.0.1:7777"), "base URL of the sbx-api service")

	root.AddCommand(newDeployCommand())

	var (
		createTemplate  string
		createNamespace string
	)
	createCmd := &cobra.Command{
		Use:   "create <id>",
		Short: "Create and start a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := []sandbox.CreateOption{}
			if createTemplate != "" {
				opts = append(opts, sandbox.WithTemplate(createTemplate))
			}
			if createNamespace != "" {
				opts = append(opts, sandbox.WithNamespace(createNamespace))
			}
			_, err := client.Create(cmd.Context(), args[0], opts...)
			return err
		},
	}
	createCmd.Flags().StringVar(&createTemplate, "template", "sandbox", "ActorTemplate name")
	createCmd.Flags().StringVar(&createNamespace, "namespace", "ate-sandbox", "Kubernetes namespace of the ActorTemplate")
	root.AddCommand(createCmd)

	// Top-level legacy commands (sbx <command> <id> ...)
	root.AddCommand(&cobra.Command{
		Use:   "suspend <id>",
		Short: "Snapshot to external storage and free the worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Suspend(cmd.Context())
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "resume <id>",
		Short: "Resume from the latest snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Resume(cmd.Context())
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Delete(cmd.Context())
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "cmd <id> <cmdline>",
		Short: "Run a shell command line in the sandbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := client.Sandbox(args[0]).Cmd(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			if res.Stdout != "" {
				fmt.Println(strings.TrimSuffix(res.Stdout, "\n"))
			}
			if res.Stderr != "" {
				fmt.Fprintln(os.Stderr, strings.TrimSuffix(res.Stderr, "\n"))
			}
			if res.TimedOut {
				fmt.Fprintln(os.Stderr, "sbx: command timed out")
			}
			if res.ExitCode != 0 {
				os.Exit(res.ExitCode)
			}
			return nil
		},
	})

	fsCmd := &cobra.Command{
		Use:   "fs",
		Short: "Operate on files and directories in a sandbox",
	}
	fsCmd.AddCommand(&cobra.Command{
		Use:   "read <id> <path>",
		Short: "Print a sandbox file to stdout",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := client.Sandbox(args[0]).ReadFile(cmd.Context(), args[1])
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
		Short: "Write stdin to a sandbox file",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).WriteFile(cmd.Context(), args[1], os.Stdin, 0o644)
		},
	})
	fsCmd.AddCommand(&cobra.Command{
		Use:   "ls <id> <path>",
		Short: "List a sandbox directory",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := client.Sandbox(args[0]).ListDir(cmd.Context(), args[1])
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
		Short: "Stat a sandbox path",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := client.Sandbox(args[0]).Stat(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			fmt.Printf("path:  %s\nmode:  %s\nsize:  %d\nmtime: %s\n", e.Path, e.ModeString, e.Size, e.ModTime)
			return nil
		},
	})
	fsCmd.AddCommand(&cobra.Command{
		Use:   "rm <id> <path>",
		Short: "Delete a file or directory in the sandbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Remove(cmd.Context(), args[1])
		},
	})
	fsCmd.AddCommand(&cobra.Command{
		Use:   "mkdir <id> <path>",
		Short: "Create a directory in the sandbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Mkdir(cmd.Context(), args[1], 0o755)
		},
	})
	root.AddCommand(fsCmd)

	// Dynamically register sandbox instance subcommands (sbx <id> ...)
	cmd, remainingArgs, _ := root.Find(os.Args[1:])
	if cmd == root && len(remainingArgs) > 0 {
		id := remainingArgs[0]
		if !strings.HasPrefix(id, "-") {
			root.AddCommand(newSandboxCommand(id, &client))
		}
	}

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "sbx:", err)
		os.Exit(1)
	}
}

func newSandboxCommand(id string, client **sandbox.Client) *cobra.Command {
	sbCmd := &cobra.Command{
		Use:   id,
		Short: fmt.Sprintf("Operate on sandbox %s", id),
	}

	sbCmd.AddCommand(&cobra.Command{
		Use:   "cmd <cmdline>",
		Short: "Run a shell command line in the sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := (*client).Sandbox(id).Cmd(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if res.Stdout != "" {
				fmt.Println(strings.TrimSuffix(res.Stdout, "\n"))
			}
			if res.Stderr != "" {
				fmt.Fprintln(os.Stderr, strings.TrimSuffix(res.Stderr, "\n"))
			}
			if res.TimedOut {
				fmt.Fprintln(os.Stderr, "sbx: command timed out")
			}
			if res.ExitCode != 0 {
				os.Exit(res.ExitCode)
			}
			return nil
		},
	})

	fsCmd := &cobra.Command{
		Use:   "fs",
		Short: "Operate on files and directories in the sandbox",
	}

	fsCmd.AddCommand(&cobra.Command{
		Use:   "read <path>",
		Short: "Print a sandbox file to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := (*client).Sandbox(id).ReadFile(cmd.Context(), args[0])
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
		Short: "Write stdin to a sandbox file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return (*client).Sandbox(id).WriteFile(cmd.Context(), args[0], os.Stdin, 0o644)
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "ls <path>",
		Short: "List a sandbox directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := (*client).Sandbox(id).ListDir(cmd.Context(), args[0])
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
		Short: "Stat a sandbox path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := (*client).Sandbox(id).Stat(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("path:  %s\nmode:  %s\nsize:  %d\nmtime: %s\n", e.Path, e.ModeString, e.Size, e.ModTime)
			return nil
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "rm <path>",
		Short: "Delete a file or directory in the sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return (*client).Sandbox(id).Remove(cmd.Context(), args[0])
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "mkdir <path>",
		Short: "Create a directory in the sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return (*client).Sandbox(id).Mkdir(cmd.Context(), args[0], 0o755)
		},
	})

	sbCmd.AddCommand(fsCmd)
	return sbCmd
}
