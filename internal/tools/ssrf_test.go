package tools

import (
	"net/netip"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Les autres tests utilisent des serveurs httptest sur 127.0.0.1.
	allowPrivateDestinations = true
	os.Exit(m.Run())
}

func TestIsBlockedIP(t *testing.T) {
	old := allowPrivateDestinations
	allowPrivateDestinations = false
	defer func() { allowPrivateDestinations = old }()

	for _, s := range []string{"10.1.2.3", "172.16.0.1", "172.31.255.1", "192.168.1.1", "127.0.0.1", "169.254.169.254", "::1", "fe80::1", "fc00::1", "::ffff:127.0.0.1", "0.0.0.0", "100.64.0.1"} {
		if !isBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s devrait être bloquée", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "172.32.0.1", "2606:4700:4700::1111"} {
		if isBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s ne devrait pas être bloquée", s)
		}
	}
}

func TestSSRFClientBlocksLoopback(t *testing.T) {
	old := allowPrivateDestinations
	allowPrivateDestinations = false
	defer func() { allowPrivateDestinations = old }()

	if _, err := newSSRFSafeClient(0).Get("http://127.0.0.1:1/"); err == nil {
		t.Fatal("connexion loopback aurait dû être refusée")
	}
}
