package tools

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveToUnavailableWithoutPerms(t *testing.T) {
	if _, err := saveFetchedContent(nil, false, "/tmp/x", []byte("x")); err == nil {
		t.Fatal("attendu une erreur sans Perms")
	}
	if strings.Contains(string((&BrowserFetchTool{}).ParametersSchema()), "save_to") {
		t.Error(`"save_to" ne doit pas apparaître dans le schéma sans Perms`)
	}
}

func TestSaveToRejectsUnauthorizedDir(t *testing.T) {
	withSandboxReady(t, false)
	perms := NewDirPermissions(nil)
	if _, err := saveFetchedContent(perms, false, filepath.Join(t.TempDir(), "x"), []byte("x")); err == nil {
		t.Fatal("attendu un refus pour un répertoire non autorisé")
	}
}
