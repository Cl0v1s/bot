package llm

import (
	"errors"
	"unicode/utf8"
)

// maxLoopRecoveries : relances automatiques (par appel) après une boucle
// détectée.
const maxLoopRecoveries = 2

var errLoopDetected = errors.New("boucle de répétition détectée, génération coupée")

// loopNudge : rappel ajouté (à la requête seulement, pas à l'historique)
// pour relancer le modèle après une boucle.
const loopNudge = "[harnais] Ta génération précédente est partie en boucle (même passage répété) et a été interrompue. Ne reviens pas sur cette réflexion : tranche immédiatement, avec un raisonnement très bref, puis poursuis la tâche (appelle l'outil suivant ou donne ta réponse)."

// FinishReasonLoop : finish_reason synthétique (jamais envoyé par un
// serveur) d'une génération coupée par le client parce que le modèle
// répétait en boucle le même motif (voir repeatTail).
const FinishReasonLoop = "loop"

const (
	// loopMinPeriod / loopMaxPeriod : longueur (en octets) du motif répété
	// recherché — de "aaaa…" à un paragraphe entier.
	loopMinPeriod = 4
	loopMaxPeriod = 1500
	// loopMinSpan : taille minimale (octets) de la répétition en queue de
	// texte, et au moins loopMinRepeats fois le motif, pour conclure à une
	// boucle (évite de couper une liste ou un tableau légitimement
	// répétitif).
	loopMinSpan    = 300
	loopMinRepeats = 4
	// loopWindow : seule la fin du texte est examinée.
	loopWindow = 8192
	// loopCheckEvery : le test n'est refait qu'après autant d'octets neufs.
	loopCheckEvery = 64
)

// repeatTail indique si la fin de s est un motif répété en boucle, et
// retourne le nombre d'octets de la queue à retirer pour ne garder qu'une
// seule occurrence du motif.
func repeatTail(s string) (int, bool) {
	if len(s) > loopWindow {
		s = s[len(s)-loopWindow:]
	}
	n := len(s)
	for p := loopMinPeriod; p <= loopMaxPeriod && p*loopMinRepeats <= n; p++ {
		// Longueur du suffixe de période p : s[i] == s[i-p] jusqu'à rompre.
		run := 0
		for i := n - 1; i-p >= 0 && s[i] == s[i-p]; i-- {
			run++
		}
		if run >= loopMinSpan && run >= (loopMinRepeats-1)*p {
			cut := run
			// Ne pas couper au milieu d'un caractère UTF-8.
			for cut > 0 && !utf8.RuneStart(s[n-cut]) {
				cut--
			}
			return cut, true
		}
	}
	return 0, false
}

// loopGuard surveille un texte en cours de génération (réponse ou
// raisonnement).
type loopGuard struct{ checked int }

// check retourne (octets à retirer, true) quand la fin de text boucle.
func (g *loopGuard) check(text string) (int, bool) {
	if len(text)-g.checked < loopCheckEvery {
		return 0, false
	}
	g.checked = len(text)
	return repeatTail(text)
}
