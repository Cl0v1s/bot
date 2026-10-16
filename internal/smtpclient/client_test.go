package smtpclient

import "testing"

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
