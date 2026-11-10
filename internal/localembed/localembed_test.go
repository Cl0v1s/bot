package localembed

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Un texte déjà en cache n'a besoin ni du modèle ni du réseau.
func TestEmbedServesCachedVectorsWithoutModel(t *testing.T) {
	dir := t.TempDir()
	l := &Local{Dir: dir, Model: "m"}
	cache, _ := json.Marshal(map[string][]float32{"m\ta": {1, 0}, "m\tb": {0, 1}})
	if err := os.WriteFile(filepath.Join(dir, "vectors.json"), cache, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := l.Embed(context.Background(), []string{"b", "a"})
	if err != nil || len(got) != 2 || got[0][1] != 1 || got[1][0] != 1 {
		t.Fatalf("got %v, err %v", got, err)
	}
}

func TestEmbedWithoutModelConfiguredFails(t *testing.T) {
	l := &Local{Dir: t.TempDir()}
	if _, err := l.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("attendu une erreur sans modèle configuré")
	}
}

// Test réel (modèle ONNX, Go pur) : EMBED_TEST_DIR doit pointer vers un
// dossier contenant models/<modèle>/ déjà téléchargé (sinon, ignoré : le
// téléchargement fait ~130 Mo).
func TestEmbedRealModel(t *testing.T) {
	dir := os.Getenv("EMBED_TEST_DIR")
	if dir == "" {
		t.Skip("EMBED_TEST_DIR non défini")
	}
	l := &Local{Dir: t.TempDir(), Model: "Xenova/paraphrase-multilingual-MiniLM-L12-v2"}
	if err := os.Symlink(filepath.Join(dir, "models"), filepath.Join(l.Dir, "models")); err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	v, err := l.Embed(context.Background(), []string{"utilise", "emploie", "héberge"})
	if err != nil || len(v) != 3 || len(v[0]) != 384 {
		t.Fatalf("err=%v len=%d", err, len(v))
	}
	dot := func(a, b []float32) (s float32) {
		for i := range a {
			s += a[i] * b[i]
		}
		return
	}
	if dot(v[0], v[1]) <= dot(v[0], v[2]) {
		t.Errorf("« utilise » devrait être plus proche d'« emploie » (%.2f) que d'« héberge » (%.2f)", dot(v[0], v[1]), dot(v[0], v[2]))
	}
}

func TestRunQuietlyFiltersGoMLXNoiseOnly(t *testing.T) {
	r, w, _ := os.Pipe()
	orig := os.Stdout
	os.Stdout = w
	_, _ = runQuietly(func() (int, error) {
		os.Stdout.WriteString("GoMLX: dynamic axes failed for model \"x\"; retrying\n")
		os.Stdout.WriteString("autre ligne\n")
		return 0, nil
	})
	os.Stdout = orig
	w.Close()
	b, _ := io.ReadAll(r)
	if string(b) != "autre ligne\n" {
		t.Fatalf("sortie = %q", b)
	}
}
