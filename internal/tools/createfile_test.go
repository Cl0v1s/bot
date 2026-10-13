package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateAndAppend(t *testing.T) {
	withSandboxReady(t, false)
	dir := t.TempDir()
	perms := NewDirPermissions(nil)
	perms.AlwaysAllow(dir)
	path := filepath.Join(dir, "f.txt")
	args, _ := json.Marshal(map[string]any{"path": path, "content": "a\n"})
	create := &CreateFileTool{Perms: perms}
	if _, err := create.Call(context.Background(), string(args)); err != nil {
		t.Fatal(err)
	}
	if _, err := create.Call(context.Background(), string(args)); err == nil {
		t.Fatal("create_file doit refuser un fichier existant")
	}
	args, _ = json.Marshal(map[string]any{"path": path, "new_string": "b\n", "append": true})
	if _, err := (&EditFileTool{Perms: perms}).Call(context.Background(), string(args)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "a\nb\n" {
		t.Fatalf("contenu = %q", got)
	}
}
