// Package localembed calcule des embeddings sur CPU, en Go pur, avec un
// modèle ONNX local (hugot, backend Go : ni cgo, ni Node, ni Python). Le
// modèle quantifié (~130 Mo pour paraphrase-multilingual-MiniLM-L12-v2) est
// téléchargé depuis Hugging Face au premier usage dans Dir/models.
package localembed

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

const (
	// onnxFile : variante quantifiée (q8) du modèle, la plus petite.
	onnxFile        = "onnx/model_quantized.onnx"
	downloadTimeout = 15 * time.Minute
)

// Local : embedder CPU local. Dir contient le modèle téléchargé et le cache
// de vecteurs. Les ressources du modèle sont chargées au premier Embed et
// gardées ; Close les libère.
type Local struct {
	Dir   string
	Model string // dépôt Hugging Face, ex: Xenova/paraphrase-multilingual-MiniLM-L12-v2

	mu         sync.Mutex
	session    *hugot.Session
	pipeline   *pipelines.FeatureExtractionPipeline
	cache      map[string][]float32
	cacheReady bool
}

func (l *Local) path(name string) string { return filepath.Join(l.Dir, name) }

func (l *Local) modelDir() string {
	return filepath.Join(l.Dir, "models", strings.ReplaceAll(l.Model, "/", "_"))
}

// load télécharge le modèle s'il manque, puis crée la session et le pipeline.
func (l *Local) load(ctx context.Context) error {
	if l.pipeline != nil {
		return nil
	}
	if l.Model == "" {
		return fmt.Errorf("modèle d'embeddings non configuré")
	}
	dir := l.modelDir()
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(onnxFile))); err != nil {
		if err := os.MkdirAll(filepath.Join(l.Dir, "models"), 0o755); err != nil {
			return err
		}
		dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
		defer cancel()
		opts := hugot.NewDownloadOptions()
		opts.OnnxFilePath = onnxFile
		if _, err := hugot.DownloadModel(dctx, l.Model, filepath.Join(l.Dir, "models"), opts); err != nil {
			return fmt.Errorf("téléchargement du modèle d'embeddings %s: %w", l.Model, err)
		}
	}
	s, err := hugot.NewGoSession(ctx)
	if err != nil {
		return fmt.Errorf("session d'embeddings: %w", err)
	}
	p, err := hugot.NewPipeline(s, hugot.FeatureExtractionConfig{
		ModelPath: dir,
		Name:      "ontology-embeddings",
		Options:   []hugot.FeatureExtractionOption{pipelines.WithNormalization()},
	})
	if err != nil {
		_ = s.Destroy()
		return fmt.Errorf("chargement du modèle d'embeddings: %w", err)
	}
	l.session, l.pipeline = s, p
	return nil
}

// Close libère le modèle chargé.
func (l *Local) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.session == nil {
		return nil
	}
	err := l.session.Destroy()
	l.session, l.pipeline = nil, nil
	return err
}

func (l *Local) key(text string) string { return l.Model + "\t" + text }

func (l *Local) loadCache() {
	if l.cacheReady {
		return
	}
	l.cacheReady = true
	l.cache = map[string][]float32{}
	if b, err := os.ReadFile(l.path("vectors.json")); err == nil {
		_ = json.Unmarshal(b, &l.cache)
	}
}

// Embed retourne un vecteur normalisé par texte (ontology.Embedder). Les
// vecteurs sont mis en cache sur disque : les types de relation sont peu
// nombreux et reviennent sans cesse, le modèle n'est donc chargé que quand un
// texte nouveau apparaît.
func (l *Local) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadCache()
	var missing []string
	seen := map[string]bool{}
	for _, t := range inputs {
		if _, ok := l.cache[l.key(t)]; !ok && !seen[t] {
			seen[t] = true
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		if err := l.load(ctx); err != nil {
			return nil, err
		}
		// Le backend Go est conçu pour de petits lots (~32 textes).
		const batch = 32
		for i := 0; i < len(missing); i += batch {
			chunk := missing[i:min(i+batch, len(missing))]
			res, err := runQuietly(func() (*pipelines.FeatureExtractionOutput, error) {
				return l.pipeline.RunPipeline(ctx, chunk)
			})
			if err != nil {
				return nil, fmt.Errorf("embeddings: %w", err)
			}
			if len(res.Embeddings) != len(chunk) {
				return nil, fmt.Errorf("embeddings: %d vecteur(s) pour %d texte(s)", len(res.Embeddings), len(chunk))
			}
			for j, t := range chunk {
				l.cache[l.key(t)] = res.Embeddings[j]
			}
		}
		if b, err := json.Marshal(l.cache); err == nil {
			_ = os.MkdirAll(l.Dir, 0o755)
			_ = os.WriteFile(l.path("vectors.json"), b, 0o644)
		}
	}
	out := make([][]float32, len(inputs))
	for i, t := range inputs {
		out[i] = l.cache[l.key(t)]
	}
	return out, nil
}

// runQuietly exécute fn en filtrant, sur la sortie standard, la ligne
// « GoMLX: dynamic axes failed ... » : hugot l'écrit directement avec
// fmt.Printf (sans option pour la couper) la première fois qu'il compile le
// modèle, alors qu'il s'agit d'un simple repli interne qui réussit ensuite.
// Les autres lignes écrites pendant ce temps sont réémises telles quelles.
func runQuietly[T any](fn func() (T, error)) (T, error) {
	real := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return fn()
	}
	os.Stdout = w
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			if line := sc.Text(); !strings.HasPrefix(line, "GoMLX: dynamic axes failed") {
				fmt.Fprintln(real, line)
			}
		}
	}()
	defer func() {
		os.Stdout = real
		w.Close()
		<-done
		r.Close()
	}()
	return fn()
}
