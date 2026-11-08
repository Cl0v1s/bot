package smtpclient

import (
	"strings"
	"testing"
)

func TestNormalizeTLSMode(t *testing.T) {
	for in, want := range map[string]string{"": "starttls", "STARTTLS": "starttls", "tls": "tls"} {
		if got, err := NormalizeTLSMode(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"none", "NONE", "plain"} {
		if _, err := NormalizeTLSMode(in); err == nil {
			t.Errorf("%q devrait être refusé", in)
		}
	}
}

func TestSendRejectsNone(t *testing.T) {
	c := &Client{Host: "127.0.0.1", Port: "1", TLSMode: "none"}
	if err := c.Send(Mail{To: "a@b.c"}); err == nil {
		t.Fatal("mode none devrait être refusé")
	}
}

func TestBuildMessageMultipart(t *testing.T) {
	msg := string(buildMessage("a@x.org", Mail{To: "b@y.org", Subject: "s", Body: "texte é", HTML: "<p>html é</p>"}))
	for _, want := range []string{"multipart/alternative", "Content-Type: text/plain", "Content-Type: text/html", "quoted-printable", "texte =C3=A9", "<p>html =C3=A9</p>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("manque %q dans:\n%s", want, msg)
		}
	}
	plain := string(buildMessage("a@x.org", Mail{To: "b@y.org", Subject: "s", Body: "x"}))
	if !strings.Contains(plain, `Content-Type: text/plain; charset="utf-8"`) || strings.Contains(plain, "multipart") {
		t.Errorf("sans HTML, le mail doit rester text/plain:\n%s", plain)
	}
}
