package mdfmt

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

var cellTagRe = regexp.MustCompile(`<(th|td)((?:\s[^>]*)?)>`)

const (
	tableStyle = "border-collapse:collapse;margin:8px 0"
	thStyle    = "border:1px solid #c8c8c8;padding:4px 10px;background:#f2f2f2;text-align:left"
	tdStyle    = "border:1px solid #c8c8c8;padding:4px 10px;vertical-align:top"
)

// HTML convertit le markdown en document HTML autonome (styles en ligne, car
// les clients mail ignorent souvent <style>). Le HTML brut présent dans la
// source n'est pas repris (goldmark en mode sûr) : le contenu vient du modèle.
func HTML(src string) (string, error) {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return "", err
	}
	body := strings.ReplaceAll(buf.String(), "<table>", `<table style="`+tableStyle+`">`)
	body = cellTagRe.ReplaceAllStringFunc(body, func(m string) string {
		sub := cellTagRe.FindStringSubmatch(m)
		css := tdStyle
		if sub[1] == "th" {
			css = thStyle
		}
		attrs := sub[2]
		if i := strings.Index(attrs, `style="`); i >= 0 {
			j := i + len(`style="`)
			return "<" + sub[1] + attrs[:j] + css + ";" + attrs[j:] + ">"
		}
		return "<" + sub[1] + ` style="` + css + `"` + attrs + ">"
	})
	return `<!DOCTYPE html><html><head><meta charset="utf-8"></head>` +
		`<body style="font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;font-size:14px;line-height:1.5;color:#222">` +
		body + `</body></html>`, nil
}
