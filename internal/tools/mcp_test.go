package tools

import (
	"strings"
	"testing"
)

func TestMCPToolName(t *testing.T) {
	if got := mcpToolName("fs", "read.file"); got != "mcp_fs_read_file" {
		t.Fatalf("got %q", got)
	}
	if got := mcpToolName("fs", strings.Repeat("a", 100)); len(got) != 64 {
		t.Fatalf("len = %d", len(got))
	}
}

func TestLoadMCPToolsAbsentFile(t *testing.T) {
	list, closeFn := LoadMCPTools(t.Context(), t.TempDir()+"/.bot_mcp.json", func(m string) { t.Errorf("warn: %s", m) })
	defer closeFn()
	if len(list) != 0 {
		t.Fatalf("list = %v", list)
	}
}
