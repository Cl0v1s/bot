// Package smtpclient envoie des mails via SMTP en utilisant uniquement la
// stdlib (net/smtp), avec STARTTLS ou TLS implicite obligatoires (le mode
// sans chiffrement est refusé).
package smtpclient

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

type Client struct {
	Host     string
	Port     string
	User     string
	Password string
	From     string
	TLSMode  string // "starttls" | "tls"
}

type Mail struct {
	To         string
	Subject    string
	Body       string
	InReplyTo  string // Message-ID du mail auquel on répond, optionnel
	References string // en-tête References, optionnel
}

func (c *Client) addr() string {
	return net.JoinHostPort(c.Host, c.Port)
}

// Send envoie un mail texte simple. Le transport est toujours chiffré et le
// certificat du serveur vérifié : aucun identifiant ne transite en clair.
func (c *Client) Send(m Mail) error {
	msg := buildMessage(c.From, m)

	mode, err := NormalizeTLSMode(c.TLSMode)
	if err != nil {
		return err
	}
	return c.send(mode, m.To, msg)
}

// NormalizeTLSMode valide un mode TLS SMTP ("starttls" par défaut, ou "tls").
// "none" et toute autre valeur sont refusés.
func NormalizeTLSMode(mode string) (string, error) {
	switch m := strings.ToLower(strings.TrimSpace(mode)); m {
	case "", "starttls":
		return "starttls", nil
	case "tls":
		return "tls", nil
	case "none":
		return "", fmt.Errorf("SMTP_TLS_MODE=none refusé : TLS obligatoire (utiliser \"starttls\" ou \"tls\")")
	default:
		return "", fmt.Errorf("SMTP_TLS_MODE invalide %q (attendu \"starttls\" ou \"tls\")", mode)
	}
}

func (c *Client) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
}

func (c *Client) auth() smtp.Auth {
	if c.User == "" {
		return nil
	}
	return smtp.PlainAuth("", c.User, c.Password, c.Host)
}

// send ouvre la connexion (TLS implicite, ou STARTTLS obligatoire) et
// n'authentifie qu'une fois le canal chiffré et vérifié.
func (c *Client) send(mode, to string, msg []byte) error {
	dialer := &net.Dialer{Timeout: 30 * time.Second}

	var conn net.Conn
	var err error
	if mode == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", c.addr(), c.tlsConfig())
	} else {
		conn, err = dialer.Dial("tcp", c.addr())
	}
	if err != nil {
		return fmt.Errorf("connexion SMTP: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return fmt.Errorf("client SMTP: %w", err)
	}
	defer client.Close()

	if mode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("le serveur SMTP n'annonce pas STARTTLS : envoi refusé (pas de transport en clair)")
		}
		if err := client.StartTLS(c.tlsConfig()); err != nil {
			return fmt.Errorf("STARTTLS SMTP: %w", err)
		}
		if _, secure := client.TLSConnectionState(); !secure {
			return fmt.Errorf("STARTTLS SMTP: connexion non chiffrée après négociation, envoi refusé")
		}
	}

	if auth := c.auth(); auth != nil {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("authentification SMTP: %w", err)
		}
	}

	if err := client.Mail(c.From); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("écriture message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("clôture message: %w", err)
	}
	return client.Quit()
}

func buildMessage(from string, m Mail) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-Id: %s\r\n", newMessageID(from))
	if m.InReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", m.InReplyTo)
	}
	if m.References != "" {
		fmt.Fprintf(&b, "References: %s\r\n", m.References)
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("\r\n")
	b.WriteString(m.Body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

// newMessageID génère un Message-Id RFC 5322 unique, requis pour que les
// clients mail (ex. Thunderbird) puissent chaîner correctement les réponses
// aux réponses via l'en-tête References.
func newMessageID(from string) string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])

	domain := "localhost"
	if _, domainPart, ok := strings.Cut(from, "@"); ok && domainPart != "" {
		domain = domainPart
	}
	return fmt.Sprintf("<%s.%d@%s>", hex.EncodeToString(raw[:]), time.Now().UnixNano(), domain)
}
