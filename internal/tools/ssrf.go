package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// allowPrivateDestinations désactive le filtrage. Non exportée et jamais
// modifiée en production : réservée aux tests, qui ciblent des httptest.Server
// sur 127.0.0.1.
var allowPrivateDestinations = false

// errBlockedAddress : destination refusée par la protection SSRF.
var errBlockedAddress = fmt.Errorf("adresse de destination interdite (privée, loopback ou link-local)")

// blockedPrefixes complète les prédicats de net/netip (IsPrivate, IsLoopback,
// IsLinkLocal...) avec des plages non couvertes par eux.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "ce réseau"
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT
	netip.MustParsePrefix("169.254.0.0/16"), // link-local, dont métadonnées cloud 169.254.169.254
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),  // réservé
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64
}

// isBlockedIP indique si ip est une destination interdite : privée
// (10/8, 172.16/12, 192.168/16, fc00::/7), loopback (127/8, ::1),
// link-local (169.254/16, fe80::/10), non spécifiée ou multicast.
func isBlockedIP(ip netip.Addr) bool {
	if allowPrivateDestinations {
		return false
	}
	ip = ip.Unmap()
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || !ip.IsValid() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ssrfControl est un net.Dialer.Control : appelé après résolution DNS, juste
// avant connect(), avec l'adresse IP réelle visée — couvre donc aussi les
// redirections et le DNS rebinding, que filtrer l'URL en amont ne couvrirait pas.
func ssrfControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", errBlockedAddress, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q", errBlockedAddress, host)
	}
	if isBlockedIP(ip) {
		return fmt.Errorf("%w: %s", errBlockedAddress, ip)
	}
	return nil
}

// newSSRFSafeClient retourne un client HTTP dont le dialer refuse toute
// connexion vers une adresse interne. Les proxys d'environnement sont
// ignorés (sinon la connexion visée serait celle du proxy, pas la cible).
func newSSRFSafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: ssrfControl}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// checkPublicHost résout l'hôte de l'URL et refuse s'il pointe vers une
// adresse interne. Utilisé par browser_fetch : la navigation est faite par un
// navigateur externe dont on ne contrôle pas le dialer, donc c'est une
// vérification en amont (ne couvre pas redirections/rebinding côté navigateur).
func checkPublicHost(ctx context.Context, host string) error {
	if ip, err := netip.ParseAddr(host); err == nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("%w: %s", errBlockedAddress, ip)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("résolution de %q: %w", host, err)
	}
	for _, a := range addrs {
		if isBlockedIP(a) {
			return fmt.Errorf("%w: %s -> %s", errBlockedAddress, host, a)
		}
	}
	return nil
}
