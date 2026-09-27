package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPGetSaveToWritesFullContent(t *testing.T) {
	big := strings.Repeat("ligne de données\n", 5000) // > MaxBodyBytes par défaut
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, big)
	}))
	defer srv.Close()

	withSandboxReady(t, false)
	dir := t.TempDir()
	perms := NewDirPermissions(nil)
	perms.AlwaysAllow(dir)
	dest := filepath.Join(dir, "sub", "page.txt")

	out, err := (&HTTPGetTool{Perms: perms}).Call(context.Background(), `{"url":"`+srv.URL+`","save_to":"`+dest+`"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != big {
		t.Errorf("contenu enregistré tronqué/altéré (%d octets au lieu de %d)", len(got), len(big))
	}
	if len(out) > 2000 {
		t.Errorf("la réponse ne devrait contenir qu'un résumé, pas le contenu (%d octets)", len(out))
	}
}

func TestSaveToUnavailableWithoutPerms(t *testing.T) {
	if _, err := saveFetchedContent(nil, false, "/tmp/x", []byte("x")); err == nil {
		t.Fatal("attendu une erreur sans Perms")
	}
	if strings.Contains(string((&HTTPGetTool{}).ParametersSchema()), "save_to") {
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
