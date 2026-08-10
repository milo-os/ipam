package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/component-base/cli"

	"go.miloapis.com/ipam/internal/version"
)

func main() {
	command := NewIPAMServerCommand()
	code := cli.Run(command)
	os.Exit(code)
}

// NewIPAMServerCommand creates the root command with subcommands for the
// IPAM server.
func NewIPAMServerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ipam",
		Short: "IPAM service apiserver",
		Long: `IPAM is a Kubernetes-native IP Address Management service.

It provides synchronous CIDR and IP allocation through IPPool, IPClaim,
IPAllocation, ASNPool, and ASNClaim resources exposed as an aggregated
Kubernetes API server.`,
	}

	cmd.AddCommand(NewServeCommand())
	cmd.AddCommand(NewMigrateCommand())
	cmd.AddCommand(NewReclaimCommand())
	cmd.AddCommand(NewVersionCommand())

	return cmd
}

// NewVersionCommand prints build information.
func NewVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Run: func(cmd *cobra.Command, args []string) {
			info := version.Get()
			fmt.Printf("IPAM Server\n")
			fmt.Printf("  Version:       %s\n", info.Version)
			fmt.Printf("  Git Commit:    %s\n", info.GitCommit)
			fmt.Printf("  Git Tree:      %s\n", info.GitTreeState)
			fmt.Printf("  Build Date:    %s\n", info.BuildDate)
			fmt.Printf("  Go Version:    %s\n", info.GoVersion)
			fmt.Printf("  Go Compiler:   %s\n", info.Compiler)
			fmt.Printf("  Platform:      %s\n", info.Platform)
		},
	}
}
