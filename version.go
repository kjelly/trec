package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// appVersion is replaced by release builds with -ldflags.
var appVersion = "dev"

func newVersionCommand() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print the trec version", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) {
		fmt.Printf("trec %s\n", appVersion)
	}}
}
