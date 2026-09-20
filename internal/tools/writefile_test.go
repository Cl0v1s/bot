package tools

import (
	"context"
	"strings"
	"testing"
)

// Un chemin relatif doit être refusé, sans jamais résoudre contre le
// répertoire de travail du processus (ambigu, non garanti côté modèle).
func TestWriteFileRejectsRelativePath(t *testing.T) {
	tool := &WriteFileTool{Perms: NewDirPermissions(nil)}
	_, err := tool.Call(context.Background(), `{"path":"notes/scenario.txt","content":"x"}`)
	if err == nil {
		t.Fatalf("attendu une erreur pour un chemin relatif")
	}
	if !strings.Contains(err.Error(), "absolu") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne le caractère absolu requis", err.Error())
	}
}
