package mailbot

import "testing"

func TestDomainsAligned(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"example.com", "example.com", true},
		{"mail.example.com", "example.com", true},
		{"example.com", "mail.example.com", true},
		{"evilexample.com", "example.com", false},
	} {
		if got := domainsAligned(c.a, c.b); got != c.want {
			t.Errorf("domainsAligned(%q,%q)=%v", c.a, c.b, got)
		}
	}
}

func TestVerifyDKIMUnsigned(t *testing.T) {
	raw := []byte("From: a@example.com\r\nSubject: x\r\n\r\nhi\r\n")
	if _, err := verifyDKIM(raw, "a@example.com"); err == nil {
		t.Fatal("mail non signé accepté")
	}
}
