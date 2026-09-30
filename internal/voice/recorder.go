package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strings"
	"time"
)

// Format audio capturé puis envoyé au STT : PCM 16 bits signé, mono,
// 16 kHz — ce qu'attendent les modèles de dictée (Qwen3-ASR, Whisper),
// pas de rééchantillonnage côté serveur.
const (
	sampleRate     = 16000
	bytesPerSample = 2
	// chunkDuration : granularité de la lecture, donc de la détection de
	// silence et de la réactivité à un arrêt.
	chunkDuration = 100 * time.Millisecond
)

// DefaultRecordCmd : capture PipeWire en PCM brut sur la sortie standard.
var DefaultRecordCmd = []string{"pw-record", "--raw", "--format", "s16", "--rate", "16000", "--channels", "1", "-"}

// Recorder capture le micro via une commande externe (DefaultRecordCmd par
// défaut) qui écrit du PCM brut au format ci-dessus sur sa sortie standard.
type Recorder struct {
	Cmd []string
	// MaxDuration : durée au-delà de laquelle l'enregistrement s'arrête de
	// lui-même (garde-fou contre un raccourci oublié). <= 0 = pas de limite.
	MaxDuration time.Duration
	// SilenceStop : arrêt automatique après ce temps de silence continu,
	// une fois qu'on a déjà entendu de la voix. <= 0 = désactivé : seul un
	// stop explicite (ou MaxDuration) arrête l'enregistrement.
	SilenceStop time.Duration
	// SilenceThreshold : niveau RMS (échelle int16, 0-32767) sous lequel
	// un morceau est considéré silencieux.
	SilenceThreshold float64
}

// Record enregistre jusqu'à la fermeture de stop, l'annulation de ctx, la
// durée maximale ou un silence prolongé (voir Recorder). heard indique si
// au moins un morceau a dépassé SilenceThreshold : sinon l'enregistrement
// ne contient vraisemblablement que du bruit de fond, que les modèles de
// dictée ont tendance à "transcrire" en phrases inventées.
func (r Recorder) Record(ctx context.Context, stop <-chan struct{}) (pcm []byte, heard bool, err error) {
	args := r.Cmd
	if len(args) == 0 {
		args = DefaultRecordCmd
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, args[0], args[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("lancement de %s : %w", args[0], err)
	}

	chunkBytes := int(chunkDuration.Seconds() * sampleRate * bytesPerSample)
	chunks := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, chunkBytes)
			n, err := io.ReadFull(stdout, buf)
			if n > 0 {
				select {
				case chunks <- buf[:n]:
				case <-cctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	var maxTimer <-chan time.Time
	if r.MaxDuration > 0 {
		t := time.NewTimer(r.MaxDuration)
		defer t.Stop()
		maxTimer = t.C
	}
	var silentFor time.Duration

loop:
	for {
		select {
		case <-stop:
			break loop
		case <-ctx.Done():
			break loop
		case <-maxTimer:
			break loop
		case chunk, ok := <-chunks:
			if !ok {
				// La commande s'est arrêtée d'elle-même : sans rien avoir
				// capté, c'est un échec (périphérique absent, PipeWire
				// injoignable...).
				if len(pcm) == 0 {
					cancel()
					_ = cmd.Wait()
					msg := strings.TrimSpace(stderr.String())
					if msg == "" {
						select {
						case e := <-readErr:
							msg = e.Error()
						default:
							msg = "aucune sortie"
						}
					}
					return nil, false, fmt.Errorf("%s s'est arrêté sans rien enregistrer : %s", args[0], msg)
				}
				break loop
			}
			pcm = append(pcm, chunk...)
			if rms(chunk) >= r.SilenceThreshold {
				heard = true
				silentFor = 0
			} else if heard {
				silentFor += chunkDuration
				if r.SilenceStop > 0 && silentFor >= r.SilenceStop {
					break loop
				}
			}
		}
	}
	cancel()
	_ = cmd.Wait()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(pcm) == 0 {
		return nil, false, errors.New("aucun son capté")
	}
	return pcm, heard, nil
}

// rms retourne le niveau efficace d'un morceau de PCM 16 bits petit-boutiste.
func rms(pcm []byte) float64 {
	n := len(pcm) / bytesPerSample
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
		sum += s * s
	}
	return math.Sqrt(sum / float64(n))
}

// encodeWAV enveloppe du PCM 16 bits mono 16 kHz dans un en-tête WAV
// (RIFF) minimal de 44 octets.
func encodeWAV(pcm []byte) []byte {
	out := make([]byte, 44, 44+len(pcm))
	le := binary.LittleEndian
	copy(out[0:], "RIFF")
	le.PutUint32(out[4:], uint32(36+len(pcm)))
	copy(out[8:], "WAVEfmt ")
	le.PutUint32(out[16:], 16) // taille du bloc fmt
	le.PutUint16(out[20:], 1)  // PCM
	le.PutUint16(out[22:], 1)  // mono
	le.PutUint32(out[24:], sampleRate)
	le.PutUint32(out[28:], sampleRate*bytesPerSample)
	le.PutUint16(out[32:], bytesPerSample)
	le.PutUint16(out[34:], 8*bytesPerSample)
	copy(out[36:], "data")
	le.PutUint32(out[40:], uint32(len(pcm)))
	return append(out, pcm...)
}
