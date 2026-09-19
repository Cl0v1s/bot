package mailbot

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
)

type parsedMail struct {
	From       string
	Subject    string
	MessageID  string
	References string
	Body       string
}

func parseRaw(raw []byte) (*parsedMail, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}

	header := m.Header
	body, err := extractBody(header, m.Body)
	if err != nil {
		return nil, err
	}

	return &parsedMail{
		From:       header.Get("From"),
		Subject:    decodeHeader(header.Get("Subject")),
		MessageID:  header.Get("Message-Id"),
		References: header.Get("References"),
		Body:       body,
	}, nil
}

func decodeHeader(s string) string {
	dec := new(mime.WordDecoder)
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

func extractAddress(from string) string {
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return from
	}
	return addr.Address
}

func extractBody(header mail.Header, body io.Reader) (string, error) {
	contentType := header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain; charset=us-ascii"
	}

	plain, html, err := walkParts(contentType, header.Get("Content-Transfer-Encoding"), body)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(plain) != "" {
		return strings.TrimSpace(plain), nil
	}
	if strings.TrimSpace(html) != "" {
		return stripHTML(html), nil
	}
	return "", nil
}

// walkParts parcourt récursivement un corps de mail (potentiellement
// multipart imbriqué) et retourne le premier texte brut et le premier HTML
// trouvés. Les pièces jointes et autres types sont ignorés.
func walkParts(contentType, transferEncoding string, r io.Reader) (plain, html string, err error) {
	mediaType, params, perr := mime.ParseMediaType(contentType)
	if perr != nil {
		raw, _ := io.ReadAll(r)
		return decodeTransfer(transferEncoding, raw), "", nil
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(r, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return plain, html, err
			}
			p, h, err := walkParts(part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), part)
			if err != nil {
				return plain, html, err
			}
			if plain == "" {
				plain = p
			}
			if html == "" {
				html = h
			}
		}
		return plain, html, nil
	}

	// Pièce jointe : content-disposition attachment -> ignorée.
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", "", err
	}
	decoded := decodeTransfer(transferEncoding, raw)

	switch {
	case strings.HasPrefix(mediaType, "text/html"):
		return "", decoded, nil
	case strings.HasPrefix(mediaType, "text/plain"):
		return decoded, "", nil
	default:
		return "", "", nil
	}
}

func decodeTransfer(encoding string, raw []byte) string {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		if err != nil {
			return string(raw)
		}
		return string(out)
	case "base64":
		cleaned := strings.NewReplacer("\r", "", "\n", "").Replace(string(raw))
		out, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return string(raw)
		}
		return string(out)
	default:
		return string(raw)
	}
}

// stripHTML retire grossièrement les balises HTML pour obtenir un texte approximatif.
func stripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
