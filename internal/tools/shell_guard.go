package tools

import (
	"regexp"
	"strings"
)

// shellSegmentSeparators découpe une commande en segments "logiques" (un par
// sous-commande séparée par ;, &&, || ou un saut de ligne). Les motifs
// dangereux sont vérifiés segment par segment plutôt que sur la ligne
// entière, pour limiter les faux positifs : "curl -f https://... && rm
// fichier.tmp" ne doit pas être bloqué à cause du -f de curl. Le simple `|`
// (pipe) n'est volontairement PAS un séparateur ici : il fait partie de la
// syntaxe de la bombe fork ci-dessous, qui doit être détectée sur la
// commande complète, non segmentée.
var shellSegmentSeparators = regexp.MustCompile(`&&|\|\||[;\n]`)

type dangerousShellPattern struct {
	re    *regexp.Regexp
	label string
}

// segmentPatterns : motifs vérifiés indépendamment sur chaque segment (voir
// shellSegmentSeparators). Filtre heuristique sur le texte de la commande —
// PAS un sandbox : contournable par un attaquant motivé (obfuscation,
// variables, alias, interpréteur intermédiaire comme `python -c`...). La
// vraie protection reste de ne jamais exposer ce tool à un contexte non
// supervisé (voir README : run_shell n'est jamais proposé en mode mail).
var segmentPatterns = []dangerousShellPattern{
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?rm\s+(\S+\s+)*-\w*r\w*f\w*(\s|$)`), "rm récursif et forcé (-rf)"},
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?rm\s+(\S+\s+)*-\w*f\w*r\w*(\s|$)`), "rm récursif et forcé (-fr)"},
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?rm\s+.*--recursive\b.*--force\b`), "rm récursif et forcé (--recursive --force)"},
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?rm\s+.*--force\b.*--recursive\b`), "rm récursif et forcé (--force --recursive)"},
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?rm\s+(\S+\s+)*-\w*f\w*(\s|$)`), "rm forcé (-f)"},
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?rm\s+.*--force\b`), "rm forcé (--force)"},
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?mkfs(\.\w+)?\b`), "formatage de disque (mkfs)"},
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?dd\s+.*\bof=`), "écriture disque brute (dd)"},
	{regexp.MustCompile(`(?i)>\s*/dev/(sd|nvme|hd|disk)\w*`), "écriture directe sur un périphérique disque"},
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?wipefs\b`), "effacement de partition (wipefs)"},
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?shred\b`), "effacement sécurisé irréversible (shred)"},
	{regexp.MustCompile(`(?i)^\s*(sudo\s+)?(shutdown|reboot|halt|poweroff)\b`), "arrêt/redémarrage de la machine"},
}

// wholeCommandPatterns : motifs vérifiés sur la commande complète, non
// segmentée (typiquement parce qu'ils contiennent eux-mêmes des séparateurs
// comme `|`).
var wholeCommandPatterns = []dangerousShellPattern{
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), "bombe fork"},
}

// checkDangerousShellCommand retourne (true, libellé, segment fautif) dès
// qu'un motif dangereux connu est détecté dans cmd.
func checkDangerousShellCommand(cmd string) (bool, string, string) {
	for _, p := range wholeCommandPatterns {
		if p.re.MatchString(cmd) {
			return true, p.label, strings.TrimSpace(cmd)
		}
	}

	for _, segment := range shellSegmentSeparators.Split(cmd, -1) {
		seg := strings.TrimSpace(segment)
		if seg == "" {
			continue
		}
		for _, p := range segmentPatterns {
			if p.re.MatchString(seg) {
				return true, p.label, seg
			}
		}
		if dangerous, label := isRecursivePermissionChangeOnRoot(seg); dangerous {
			return true, label, seg
		}
	}
	return false, "", ""
}

// isRecursivePermissionChangeOnRoot détecte chmod/chown récursif visant la
// racine du système ("/" comme argument isolé), ex: "chmod -R 777 /". Écrit
// comme une analyse de tokens plutôt qu'une regex : le mode/owner ("777",
// "nobody"...) s'intercale entre le flag et la cible, ce qu'une regex simple
// gère mal de façon fiable.
func isRecursivePermissionChangeOnRoot(seg string) (bool, string) {
	fields := strings.Fields(seg)
	if len(fields) > 0 && strings.EqualFold(fields[0], "sudo") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return false, ""
	}

	cmd := strings.ToLower(fields[0])
	if cmd != "chmod" && cmd != "chown" {
		return false, ""
	}

	recursive := false
	root := false
	for _, f := range fields[1:] {
		switch {
		case f == "/":
			root = true
		case f == "--recursive":
			recursive = true
		case strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.Contains(f, "R"):
			recursive = true
		}
	}

	if recursive && root {
		if cmd == "chown" {
			return true, "changement récursif du propriétaire à la racine du système"
		}
		return true, "changement récursif des droits à la racine du système"
	}
	return false, ""
}
