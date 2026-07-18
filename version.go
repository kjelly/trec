package main

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// appVersion is replaced by release builds with -ldflags.
var appVersion = "dev"

type buildMetadata struct {
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
	Time     string `json:"time,omitempty"`
	Modified bool   `json:"modified,omitempty"`
}

func currentBuildMetadata() buildMetadata {
	meta := buildMetadata{Version: appVersion}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return meta
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			meta.Revision = setting.Value
		case "vcs.time":
			meta.Time = setting.Value
		case "vcs.modified":
			meta.Modified = setting.Value == "true"
		}
	}
	return meta
}

func (m buildMetadata) DisplayVersion() string {
	if m.Version != "" && m.Version != "dev" {
		return m.Version
	}
	if m.Revision == "" {
		return "dev"
	}
	revision := m.Revision
	if len(revision) > 12 {
		revision = revision[:12]
	}
	version := "dev+" + revision
	if m.Modified {
		version += ".dirty"
	}
	return version
}

func newVersionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the trec version and source revision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, _ := cmd.Flags().GetString("format")
			meta := currentBuildMetadata()
			switch strings.ToLower(format) {
			case "text":
				fmt.Printf("trec %s", meta.DisplayVersion())
				if meta.Revision != "" {
					fmt.Printf(" (revision %s", meta.Revision)
					if meta.Modified {
						fmt.Print(", modified")
					}
					if meta.Time != "" {
						fmt.Printf(", %s", meta.Time)
					}
					fmt.Print(")")
				}
				fmt.Println()
			case "json":
				data, err := json.MarshalIndent(meta, "", "  ")
				if err != nil {
					return fmt.Errorf("encode version: %w", err)
				}
				fmt.Println(string(data))
			default:
				return fmt.Errorf("unsupported format %q (choose from text, json)", format)
			}
			return nil
		},
	}
	cmd.Flags().String("format", "text", "output format (text, json)")
	return cmd
}
