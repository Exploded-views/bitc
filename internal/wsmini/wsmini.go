// Package wsmini is the minimal RFC 6455 framing the Sprites exec channel needs, so the module
// takes no WebSocket dependency. It has a client (Dial) and a server side (Upgrade, for the
// Sprites fake): binary and text messages, fragmentation, ping/pong, close, masking rules and a
// message size limit. There is no compression, no extension and no subprotocol.
package wsmini

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 mandates SHA-1 for the accept key; it is not a security use
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Opcodes.
const (
	OpText   = 0x1
	OpBinary = 0x2
	OpClose  = 0x8
	OpPing   = 0x9
	OpPong   = 0xA
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// DefaultMaxMessage bounds one reassembled message.
const DefaultMaxMessage = 16 << 20

var (
	ErrTooBig   = errors.New("wsmini: message over the size limit")
	ErrProtocol = errors.New("wsmini: protocol error")
	// ErrClosed is returned by ReadMessage once the peer sent a close frame.
	ErrClosed = errors.New("wsmini: closed by peer")
)

// DialError: the connection could not be made, so no request byte was sent.
type DialError struct{ Err error }

func (e *DialError) Error() string { return "websocket dial: " + e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

// StatusError: the server answered the upgrade request with an HTTP status other than 101.
type StatusError struct {
	Code int
	Body []byte // at most 64 KiB
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("websocket upgrade refused: HTTP %d", e.Code)
}

// HandshakeError: the upgrade request was written (the server may have acted on it), and the
// exchange then failed without an HTTP error status: a timeout, a network error, a bad answer.
type HandshakeError struct{ Err error }

func (e *HandshakeError) Error() string { return "websocket handshake: " + e.Err.Error() }
func (e *HandshakeError) Unwrap() error { return e.Err }

// Conn is one WebSocket connection. ReadMessage must be called from one goroutine at a time;
// WriteMessage is safe for concurrent use.
type Conn struct {
	c       net.Conn
	br      *bufio.Reader
	client  bool
	max     int
	wmu     sync.Mutex
	closeMu sync.Once
}

// DialOptions: Header is added to the upgrade request; MaxMessage defaults to
// DefaultMaxMessage; HandshakeTimeout bounds the wait for the upgrade answer (default 30s);
// TLS is used for https and wss URLs (nil: the system roots).
type DialOptions struct {
	Header           http.Header
	MaxMessage       int
	HandshakeTimeout time.Duration
	TLS              *tls.Config
}

// Dial opens rawURL (http, https, ws or wss) and upgrades it. It never uses a proxy.
func Dial(ctx context.Context, rawURL string, o DialOptions) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, &DialError{err}
	}
	secure := false
	switch u.Scheme {
	case "http", "ws":
	case "https", "wss":
		secure = true
	default:
		return nil, &DialError{fmt.Errorf("unsupported scheme %q", u.Scheme)}
	}
	host := u.Host
	if u.Port() == "" {
		port := "80"
		if secure {
			port = "443"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, &DialError{err}
	}
	if secure {
		cfg := o.TLS
		if cfg == nil {
			cfg = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			cfg = cfg.Clone()
		}
		if cfg.ServerName == "" {
			cfg.ServerName = u.Hostname()
		}
		tc := tls.Client(nc, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, &DialError{err}
		}
		nc = tc
	}
	if err := ctx.Err(); err != nil {
		nc.Close()
		return nil, &DialError{err}
	}
	var kb [16]byte
	if _, err := rand.Read(kb[:]); err != nil {
		nc.Close()
		return nil, &DialError{err}
	}
	key := base64.StdEncoding.EncodeToString(kb[:])
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", u.RequestURI(), u.Host, key)
	for k, vs := range o.Header {
		for _, v := range vs {
			if strings.ContainsAny(k+v, "\r\n") {
				nc.Close()
				return nil, &DialError{errors.New("header with a line break")}
			}
			fmt.Fprintf(&req, "%s: %s\r\n", k, v)
		}
	}
	req.WriteString("\r\n")
	// From the first written byte on the server may have acted: a failure is a HandshakeError.
	to := o.HandshakeTimeout
	if to <= 0 {
		to = 30 * time.Second
	}
	_ = nc.SetDeadline(time.Now().Add(to))
	stop := context.AfterFunc(ctx, func() { _ = nc.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if _, err := io.WriteString(nc, req.String()); err != nil {
		nc.Close()
		return nil, &HandshakeError{err}
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		nc.Close()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, &HandshakeError{err}
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		nc.Close()
		return nil, &StatusError{Code: resp.StatusCode, Body: b}
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		nc.Close()
		return nil, &HandshakeError{errors.New("bad upgrade answer")}
	}
	_ = nc.SetDeadline(time.Time{})
	max := o.MaxMessage
	if max <= 0 {
		max = DefaultMaxMessage
	}
	return &Conn{c: nc, br: br, client: true, max: max}, nil
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + guid)) //nolint:gosec // see the import
	return base64.StdEncoding.EncodeToString(h[:])
}

// Upgrade is the server side: it checks the upgrade request, hijacks the connection and answers
// 101. A request that is not a valid upgrade is answered 400 and an error is returned.
func Upgrade(w http.ResponseWriter, r *http.Request, maxMessage int) (*Conn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" || key == "" {
		http.Error(w, "not a websocket upgrade", http.StatusBadRequest)
		return nil, errors.New("wsmini: not a websocket upgrade")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return nil, errors.New("wsmini: response writer cannot hijack")
	}
	nc, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	_, err = fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", acceptKey(key))
	if err == nil {
		err = brw.Flush()
	}
	if err != nil {
		nc.Close()
		return nil, err
	}
	if maxMessage <= 0 {
		maxMessage = DefaultMaxMessage
	}
	return &Conn{c: nc, br: brw.Reader, max: maxMessage}, nil
}

// SetReadDeadline passes through to the connection.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.c.SetReadDeadline(t) }

// RemoteAddr is the peer's address.
func (c *Conn) RemoteAddr() net.Addr { return c.c.RemoteAddr() }

// ReadMessage returns the next text or binary message. Pings are answered and pongs dropped
// meanwhile. A close frame is answered and returns ErrClosed.
func (c *Conn) ReadMessage() (op int, payload []byte, err error) {
	var msg []byte
	msgOp := 0
	for {
		fin, fop, data, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch fop {
		case OpPing:
			if err := c.writeFrame(OpPong, data); err != nil {
				return 0, nil, err
			}
			continue
		case OpPong:
			continue
		case OpClose:
			_ = c.writeFrame(OpClose, data)
			return 0, nil, ErrClosed
		case OpText, OpBinary:
			if msgOp != 0 {
				return 0, nil, fmt.Errorf("%w: new message inside a fragmented one", ErrProtocol)
			}
			msgOp = fop
		case 0:
			if msgOp == 0 {
				return 0, nil, fmt.Errorf("%w: continuation without a message", ErrProtocol)
			}
		default:
			return 0, nil, fmt.Errorf("%w: opcode %d", ErrProtocol, fop)
		}
		if len(msg)+len(data) > c.max {
			return 0, nil, ErrTooBig
		}
		msg = append(msg, data...)
		if fin {
			return msgOp, msg, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op int, data []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, int(h[0]&0x0f)
	if h[0]&0x70 != 0 {
		err = fmt.Errorf("%w: reserved bits set", ErrProtocol)
		return
	}
	masked := h[1]&0x80 != 0
	if masked == c.client { // a client reads unmasked frames, a server masked ones
		err = fmt.Errorf("%w: wrong masking", ErrProtocol)
		return
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= OpClose && (n > 125 || !fin) {
		err = fmt.Errorf("%w: bad control frame", ErrProtocol)
		return
	}
	if n > uint64(c.max) {
		err = ErrTooBig
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	data = make([]byte, n)
	if _, err = io.ReadFull(c.br, data); err != nil {
		return
	}
	if masked {
		for i := range data {
			data[i] ^= mask[i%4]
		}
	}
	return
}

// WriteMessage sends p as one frame (masked on a client).
func (c *Conn) WriteMessage(op int, p []byte) error {
	if op != OpText && op != OpBinary {
		return fmt.Errorf("%w: WriteMessage takes text or binary", ErrProtocol)
	}
	return c.writeFrame(op, p)
}

// Ping sends a ping frame.
func (c *Conn) Ping(p []byte) error { return c.writeFrame(OpPing, p) }

func (c *Conn) writeFrame(op int, p []byte) error {
	if op >= OpClose && len(p) > 125 {
		return fmt.Errorf("%w: control payload over 125 bytes", ErrProtocol)
	}
	hdr := make([]byte, 2, 14)
	hdr[0] = 0x80 | byte(op)
	var mb byte
	if c.client {
		mb = 0x80
	}
	switch {
	case len(p) < 126:
		hdr[1] = mb | byte(len(p))
	case len(p) < 1<<16:
		hdr[1] = mb | 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(len(p)))
	default:
		hdr[1] = mb | 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(len(p)))
	}
	body := p
	if c.client {
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		hdr = append(hdr, mask[:]...)
		body = make([]byte, len(p))
		for i := range p {
			body[i] = p[i] ^ mask[i%4]
		}
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.c.Write(hdr); err != nil {
		return err
	}
	_, err := c.c.Write(body)
	return err
}

// Close sends a normal close frame (best effort) and closes the connection.
func (c *Conn) Close() error {
	var err error
	c.closeMu.Do(func() {
		_ = c.writeFrame(OpClose, []byte{0x03, 0xe8})
		err = c.c.Close()
	})
	return err
}

// Abort closes the connection without a close frame (a dropped socket).
func (c *Conn) Abort() error {
	var err error
	c.closeMu.Do(func() { err = c.c.Close() })
	return err
}
