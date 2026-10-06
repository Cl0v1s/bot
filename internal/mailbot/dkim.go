package mailbot

import (
	"bytes"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// verifyDKIM vérifie les signatures DKIM de raw et exige qu'au moins une soit
// valide ET alignée sur le domaine de l'en-tête From (même domaine, ou
// sous-domaine/domaine parent) : une signature valide d'un domaine sans
// rapport avec l'expéditeur affiché ne prouve rien sur son identité. Retourne
// le domaine signataire retenu.
func verifyDKIM(raw []byte, from string) (string, error) {
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return "", fmt.Errorf("From illisible: %w", err)
	}
	at := strings.LastIndex(addr.Address, "@")
	if at < 0 {
		return "", errors.New("From sans domaine")
	}
	fromDomain := strings.ToLower(addr.Address[at+1:])

	verifs, err := dkim.Verify(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("vérification DKIM: %w", err)
	}
	if len(verifs) == 0 {
		return "", errors.New("aucune signature DKIM")
	}
	var problems []string
	for _, v := range verifs {
		d := strings.ToLower(v.Domain)
		switch {
		case v.Err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", d, v.Err))
		case !domainsAligned(d, fromDomain):
			problems = append(problems, fmt.Sprintf("%s: domaine non aligné avec %s", d, fromDomain))
		default:
			return d, nil
		}
	}
	return "", errors.New("signature DKIM invalide (" + strings.Join(problems, "; ") + ")")
}

// domainsAligned : alignement DKIM "relaxed" (RFC 7489), approché sans liste
// des suffixes publics — l'un des domaines est égal à l'autre ou en est un
// sous-domaine.
func domainsAligned(a, b string) bool {
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}
