// Package imapclient est un client IMAP4rev1 minimal, fait main, suffisant
// pour lister les messages non lus d'une boîte INBOX, récupérer leur contenu
// brut et les marquer comme lus. Ne vise pas l'exhaustivité du protocole.
package imapclient

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	conn    net.Conn
	r       *bufio.Reader
	tagSeq  int
	timeout time.Duration
}

// Dial se connecte au serveur IMAP. useTLS=true utilise TLS implicite
// (port 993 typiquement).
func Dial(addr string, useTLS bool) (*Client, error) {
	var conn net.Conn
	var err error

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if useTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: hostOnly(addr)})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connexion IMAP: %w", err)
	}

	c := &Client{
		conn:    conn,
		r:       bufio.NewReader(conn),
		timeout: 30 * time.Second,
	}

	// Ligne de greeting du serveur, ex: "* OK IMAP4rev1 Service Ready"
	if _, err := c.r.ReadString('\n'); err != nil {
		conn.Close()
		return nil, fmt.Errorf("greeting IMAP: %w", err)
	}
	return c, nil
}

func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) nextTag() string {
	c.tagSeq++
	return fmt.Sprintf("a%d", c.tagSeq)
}

// runCommand envoie une commande et lit les lignes de réponse jusqu'à la
// ligne étiquetée "tag OK/NO/BAD ...". Ne gère pas les littéraux {n} : à
// utiliser uniquement pour des commandes dont on sait que la réponse ne
// contient pas de littéral significatif (LOGIN, SELECT, SEARCH, STORE, LOGOUT).
func (c *Client) runCommand(cmd string) (untagged []string, status string, err error) {
	tag := c.nextTag()
	c.conn.SetDeadline(time.Now().Add(c.timeout))

	if _, err := c.conn.Write([]byte(tag + " " + cmd + "\r\n")); err != nil {
		return nil, "", err
	}

	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return untagged, "", fmt.Errorf("lecture réponse IMAP: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, tag+" ") {
			return untagged, strings.TrimPrefix(line, tag+" "), nil
		}
		untagged = append(untagged, line)
	}
}

func (c *Client) Login(user, password string) error {
	_, status, err := c.runCommand(fmt.Sprintf("LOGIN %s %s", quote(user), quote(password)))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(status, "OK") {
		return fmt.Errorf("échec LOGIN IMAP: %s", status)
	}
	return nil
}

func (c *Client) Select(mailbox string) error {
	_, status, err := c.runCommand("SELECT " + quote(mailbox))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(status, "OK") {
		return fmt.Errorf("échec SELECT %s: %s", mailbox, status)
	}
	return nil
}

var searchNumRe = regexp.MustCompile(`\d+`)

// SearchUnseen retourne les numéros de séquence des messages non lus.
func (c *Client) SearchUnseen() ([]int, error) {
	untagged, status, err := c.runCommand("SEARCH UNSEEN")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(status, "OK") {
		return nil, fmt.Errorf("échec SEARCH: %s", status)
	}

	var ids []int
	for _, line := range untagged {
		if !strings.HasPrefix(line, "* SEARCH") {
			continue
		}
		for _, tok := range searchNumRe.FindAllString(line, -1) {
			n, err := strconv.Atoi(tok)
			if err == nil {
				ids = append(ids, n)
			}
		}
	}
	return ids, nil
}

var literalRe = regexp.MustCompile(`\{(\d+)\}\s*$`)

// FetchRFC822 récupère le contenu brut (headers + corps) du message numéro seq.
func (c *Client) FetchRFC822(seq int) ([]byte, error) {
	tag := c.nextTag()
	c.conn.SetDeadline(time.Now().Add(c.timeout))

	cmd := fmt.Sprintf("%s FETCH %d (BODY.PEEK[])\r\n", tag, seq)
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return nil, err
	}

	// Première ligne : "* seq FETCH (BODY[] {size}"
	header, err := c.r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("lecture FETCH: %w", err)
	}
	header = strings.TrimRight(header, "\r\n")

	m := literalRe.FindStringSubmatch(header)
	if m == nil {
		return nil, fmt.Errorf("réponse FETCH inattendue (pas de littéral): %q", header)
	}
	size, err := strconv.Atoi(m[1])
	if err != nil {
		return nil, fmt.Errorf("taille de littéral invalide: %w", err)
	}

	buf := make([]byte, size)
	if _, err := readFull(c.r, buf); err != nil {
		return nil, fmt.Errorf("lecture corps message: %w", err)
	}

	// Consommer le reste de la réponse jusqu'à la ligne étiquetée.
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("lecture fin FETCH: %w", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, tag+" ") {
			status := strings.TrimPrefix(trimmed, tag+" ")
			if !strings.HasPrefix(status, "OK") {
				return nil, fmt.Errorf("échec FETCH: %s", status)
			}
			break
		}
	}

	return buf, nil
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// MarkSeen ajoute le flag \Seen au message numéro seq.
func (c *Client) MarkSeen(seq int) error {
	_, status, err := c.runCommand(fmt.Sprintf("STORE %d +FLAGS (\\Seen)", seq))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(status, "OK") {
		return fmt.Errorf("échec STORE: %s", status)
	}
	return nil
}

func (c *Client) Logout() error {
	_, _, err := c.runCommand("LOGOUT")
	closeErr := c.conn.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// quote encadre une chaîne IMAP "quoted string" en échappant les caractères spéciaux.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
