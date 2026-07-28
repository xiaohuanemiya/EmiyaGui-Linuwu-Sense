package server

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsClient struct {
	conn net.Conn
	read *bufio.Reader
	mu   sync.Mutex
}

type hub struct {
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
}

func newHub() *hub {
	return &hub{clients: make(map[*wsClient]struct{})}
}

func (h *hub) upgrade(w http.ResponseWriter, r *http.Request) (*wsClient, error) {
	if r.Method != http.MethodGet ||
		!headerContains(r.Header, "Connection", "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, fmt.Errorf("invalid WebSocket upgrade request")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(decoded) != 16 {
		return nil, fmt.Errorf("invalid WebSocket key")
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("server does not support connection hijacking")
	}
	connection, readWriter, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + websocketGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	if _, err := fmt.Fprintf(readWriter,
		"HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := readWriter.Flush(); err != nil {
		_ = connection.Close()
		return nil, err
	}
	client := &wsClient{conn: connection, read: readWriter.Reader}
	h.mu.Lock()
	h.clients[client] = struct{}{}
	h.mu.Unlock()
	return client, nil
}

func (h *hub) hasClients() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients) > 0
}

func (h *hub) remove(client *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[client]; ok {
		delete(h.clients, client)
		_ = client.conn.Close()
	}
	h.mu.Unlock()
}

func (h *hub) broadcast(payload []byte) {
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.RUnlock()
	for _, client := range clients {
		if err := client.writeFrame(0x1, payload); err != nil {
			h.remove(client)
		}
	}
}

func (c *wsClient) writeFrame(opcode byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	header := []byte{0x80 | opcode}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(length))
	case length <= 65535:
		header = append(header, 126, byte(length>>8), byte(length))
	default:
		header = append(header, 127, 0, 0, 0, 0,
			byte(uint64(length)>>24), byte(uint64(length)>>16), byte(uint64(length)>>8), byte(length))
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

func (c *wsClient) readLoop() error {
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		first, err := c.read.ReadByte()
		if err != nil {
			return err
		}
		second, err := c.read.ReadByte()
		if err != nil {
			return err
		}
		if first&0x80 == 0 {
			return errors.New("fragmented WebSocket frames are not supported")
		}
		opcode := first & 0x0f
		masked := second&0x80 != 0
		if !masked {
			return errors.New("client WebSocket frame was not masked")
		}
		length := uint64(second & 0x7f)
		switch length {
		case 126:
			var value uint16
			if err := binary.Read(c.read, binary.BigEndian, &value); err != nil {
				return err
			}
			length = uint64(value)
		case 127:
			if err := binary.Read(c.read, binary.BigEndian, &length); err != nil {
				return err
			}
		}
		if length > 4096 {
			return errors.New("WebSocket frame is too large")
		}
		var mask [4]byte
		if _, err := io.ReadFull(c.read, mask[:]); err != nil {
			return err
		}
		payload := make([]byte, int(length))
		if _, err := io.ReadFull(c.read, payload); err != nil {
			return err
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		switch opcode {
		case 0x8:
			_ = c.writeFrame(0x8, payload)
			return nil
		case 0x9:
			if err := c.writeFrame(0xA, payload); err != nil {
				return err
			}
		case 0xA:
			continue
		case 0x1, 0x2:
			// Telemetry is one-way. Ignore application frames from the browser.
		default:
			return errors.New("unsupported WebSocket opcode")
		}
	}
}

func headerContains(header http.Header, name, value string) bool {
	for _, item := range header.Values(name) {
		for _, part := range strings.Split(item, ",") {
			if strings.EqualFold(strings.TrimSpace(part), value) {
				return true
			}
		}
	}
	return false
}
