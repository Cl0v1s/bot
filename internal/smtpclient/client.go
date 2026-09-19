// Package smtpclient envoie des mails via SMTP en utilisant uniquement la
// stdlib (net/smtp), avec support STARTTLS, TLS implicite, ou aucune
// sécurité (réseau local de confiance).
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
	TLSMode  string // "starttls" | "tls" | "none"
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

// Send envoie un mail texte simple.
func (c *Client) Send(m Mail) error {
	msg := buildMessage(c.From, m)

	switch strings.ToLower(c.TLSMode) {
	case "tls":
		return c.sendImplicitTLS(m.To, msg)
	case "none":
		return c.sendPlain(m.To, msg)
	default: // "starttls"
		return c.sendStartTLS(m.To, msg)
	}
}

func (c *Client) auth() smtp.Auth {
	if c.User == "" {
		return nil
	}
	return smtp.PlainAuth("", c.User, c.Password, c.Host)
}

// sendStartTLS s'appuie sur smtp.SendMail, qui négocie STARTTLS automatiquement
// si le serveur l'annonce (cas standard pour le port 587).
func (c *Client) sendStartTLS(to string, msg []byte) error {
	err := smtp.SendMail(c.addr(), c.auth(), c.From, []string{to}, msg)
	if err != nil {
		return fmt.Errorf("envoi SMTP (starttls): %w", err)
	}
	return nil
}

func (c *Client) sendPlain(to string, msg []byte) error {
	err := smtp.SendMail(c.addr(), c.auth(), c.From, []string{to}, msg)
	if err != nil {
		return fmt.Errorf("envoi SMTP: %w", err)
	}
	return nil
}

// sendImplicitTLS gère le cas port 465 (TLS dès la connexion, avant tout
// échange SMTP en clair), non couvert par smtp.SendMail.
func (c *Client) sendImplicitTLS(to string, msg []byte) error {
	conn, err := tls.Dial("tcp", c.addr(), &tls.Config{ServerName: c.Host})
	if err != nil {
		return fmt.Errorf("connexion TLS SMTP: %w", err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return fmt.Errorf("client SMTP: %w", err)
	}
	defer client.Close()

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
