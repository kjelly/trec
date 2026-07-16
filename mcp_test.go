package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPTerminalSize(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		wantCols  uint16
		wantRows  uint16
		wantError string
	}{
		{name: "defaults", input: `{}`, wantCols: defaultMCPCols, wantRows: defaultMCPRows},
		{name: "custom", input: `{"cols":200,"rows":60}`, wantCols: 200, wantRows: 60},
		{name: "zero", input: `{"cols":0}`, wantError: "cols must be between"},
		{name: "negative", input: `{"rows":-1}`, wantError: "rows must be between"},
		{name: "too large", input: `{"cols":1001}`, wantError: "cols must be between"},
		{name: "fractional", input: `{"cols":80.5}`, wantError: "decode terminal size"},
		{name: "wrong type", input: `{"rows":"40"}`, wantError: "decode terminal size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size, err := mcpTerminalSize(json.RawMessage(tc.input))
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("mcpTerminalSize() error = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if size.Cols != tc.wantCols || size.Rows != tc.wantRows {
				t.Fatalf("size = %dx%d, want %dx%d", size.Cols, size.Rows, tc.wantCols, tc.wantRows)
			}
		})
	}
}

// TestMCPServerAdvertisesArrayItemSchema guards against a real
// regression: mcp.WithArray("command", mcp.Required()) alone produces
// a bare {"type":"array"} schema with no "items" — valid JSON Schema,
// but MCP clients that validate/convert tool schemas before exposing
// them (observed with Claude Code) silently dropped both "run" and
// "terminal_start" entirely rather than surfacing an error, since
// neither ever appeared in the client's own tool listing despite the
// server responding correctly to a raw tools/list probe. Every array
// property in this server's tool schemas must declare its item type.
func TestMCPServerAdvertisesArrayItemSchema(t *testing.T) {
	server := newMCPProtocolServer(&mcpServer{sessions: map[string]*mcpSession{}})
	for _, toolName := range []string{"run", "terminal_start"} {
		tool := server.GetTool(toolName)
		if tool == nil {
			t.Fatalf("%s tool is missing", toolName)
		}
		property, ok := tool.Tool.InputSchema.Properties["command"].(map[string]any)
		if !ok {
			t.Fatalf("%s property %q is missing", toolName, "command")
		}
		if property["type"] != "array" {
			t.Fatalf("%s command type = %v, want array", toolName, property["type"])
		}
		items, ok := property["items"].(map[string]any)
		if !ok {
			t.Fatalf("%s command schema has no \"items\" (this is exactly the bug that made Claude Code silently drop the tool): %#v", toolName, property)
		}
		if items["type"] != "string" {
			t.Fatalf("%s command items type = %v, want string", toolName, items["type"])
		}
	}
}

func TestMCPServerAdvertisesTerminalSize(t *testing.T) {
	server := newMCPProtocolServer(&mcpServer{sessions: map[string]*mcpSession{}})
	tool := server.GetTool("terminal_start")
	if tool == nil {
		t.Fatal("terminal_start tool is missing")
	}
	for _, name := range []string{"cols", "rows"} {
		property, ok := tool.Tool.InputSchema.Properties[name].(map[string]any)
		if !ok {
			t.Fatalf("terminal_start property %q is missing", name)
		}
		if property["type"] != "integer" {
			t.Fatalf("terminal_start property %q type = %v, want integer", name, property["type"])
		}
		if property["minimum"] != 1 || property["maximum"] != maxMCPDimension {
			t.Fatalf("terminal_start property %q bounds = %v..%v", name, property["minimum"], property["maximum"])
		}
		wantDefault := defaultMCPCols
		if name == "rows" {
			wantDefault = defaultMCPRows
		}
		if property["default"] != wantDefault {
			t.Fatalf("terminal_start property %q default = %v, want %d", name, property["default"], wantDefault)
		}
	}
}
