package tools

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"strings"
)

// wsGUID : GUID fixe du protocole WebSocket (RFC 6455 §1.3), utilisé pour
// valider la réponse Sec-WebSocket-Accept du serveur lors du handshake.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsConn est un client WebSocket minimal (RFC 6455), écrit à la main plutôt
// que via une dépendance externe (voir le commentaire de fetchWithChromeCDP)
// — suffisant pour parler au port de debug distant de Chrome (texte JSON
// uniquement, jamais de TLS : toujours localhost en clair) : pas de
// compression (RFC 7692), pas de réponse automatique aux ping, pas de
// multiplexage. Un usage strictement séquentiel (écrire une commande, lire
// les messages qui suivent) est supposé — jamais d'accès concurrent depuis
// plusieurs goroutines à la même connexion.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// dialWebSocket ouvre une connexion WebSocket vers rawURL ("ws://host:port/chemin"
// uniquement — jamais "wss://", non nécessaire pour un port de debug local).
func dialWebSocket(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("URL WebSocket invalide %q: %w", rawURL, err)
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("schéma WebSocket %q non supporté (ws uniquement)", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("connexion TCP à %q: %w", host, err)
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	path := u.Path
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("envoi de la requête d'upgrade: %w", err)
	}

	br := bufio.NewReader(conn)
	tp := textproto.NewReader(br)
	statusLine, err := tp.ReadLine()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("lecture de la réponse d'upgrade: %w", err)
	}
	if !strings.Contains(statusLine, "101") {
		conn.Close()
		return nil, fmt.Errorf("upgrade WebSocket refusé: %s", statusLine)
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("lecture des en-têtes de la réponse d'upgrade: %w", err)
	}
	if got, want := hdr.Get("Sec-Websocket-Accept"), computeWSAccept(key); got != want {
		conn.Close()
		return nil, fmt.Errorf("Sec-WebSocket-Accept invalide (%q, attendu %q) : la réponse ne vient pas d'un serveur WebSocket conforme", got, want)
	}

	return &wsConn{conn: conn, br: br}, nil
}

func computeWSAccept(key string) string {
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (c *wsConn) Close() error { return c.conn.Close() }

// WriteText envoie payload comme un unique frame texte (FIN=1), masqué comme
// l'exige RFC 6455 §5.3 pour tout frame client→serveur.
func (c *wsConn) WriteText(payload []byte) error {
	header := []byte{0x81} // FIN=1, opcode=texte(0x1)

	const maskBit = byte(0x80)
	n := len(payload)
	switch {
	case n <= 125:
		header = append(header, maskBit|byte(n))
	case n <= 0xFFFF:
		header = append(header, maskBit|126)
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header = append(header, maskBit|127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}

	var maskKey [4]byte
	if _, err := rand.Read(maskKey[:]); err != nil {
		return err
	}
	header = append(header, maskKey[:]...)

	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}

	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

// Opcodes des frames WebSocket utiles ici (RFC 6455 §11.8).
const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// ReadMessage lit un message WebSocket complet (un frame texte/binaire,
// plus ses éventuels frames de continuation, réassemblés) : ping/pong sont
// ignorés silencieusement (aucune réponse envoyée — inutile pour la durée
// de vie très courte d'une session CDP), et un frame close est traité comme
// une fin de connexion normale (io.EOF).
func (c *wsConn) ReadMessage() ([]byte, error) {
	var message []byte
	for {
		opcode, fin, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case wsOpClose:
			return nil, io.EOF
		case wsOpPing, wsOpPong:
			continue
		}
		message = append(message, payload...)
		if fin {
			return message, nil
		}
	}
}

func (c *wsConn) readFrame() (opcode int, fin bool, payload []byte, err error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(c.br, head); err != nil {
		return 0, false, nil, err
	}
	fin = head[0]&0x80 != 0
	opcode = int(head[0] & 0x0F)
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)

	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(c.br, ext); err != nil {
			return 0, false, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(c.br, ext); err != nil {
			return 0, false, nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}

	var maskKey [4]byte
	if masked { // un serveur conforme ne masque jamais (RFC 6455 §5.1) ; géré sans planter au cas où
		if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
			return 0, false, nil, err
		}
	}

	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, false, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, fin, payload, nil
}
