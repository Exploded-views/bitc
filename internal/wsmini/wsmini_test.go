package wsmini

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// echoServer upgrades and runs h on the server side connection.
func echoServer(t *testing.T, h func(*Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, 1<<20)
		if err != nil {
			return
		}
		defer func() { _ = c.Abort() }()
		h(c)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func echo(c *Conn) {
	for {
		op, p, err := c.ReadMessage()
		if err != nil {
			return
		}
		if c.WriteMessage(op, p) != nil {
			return
		}
	}
}

func TestEchoAcrossLengthClasses(t *testing.T) {
	srv := echoServer(t, echo)
	c, err := Dial(context.Background(), srv.URL, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, n := range []int{0, 1, 125, 126, 65535, 65536, 300000} {
		want := bytes.Repeat([]byte{byte(n)}, n)
		if err := c.WriteMessage(OpBinary, want); err != nil {
			t.Fatalf("%d: write: %v", n, err)
		}
		op, got, err := c.ReadMessage()
		if err != nil || op != OpBinary || !bytes.Equal(got, want) {
			t.Fatalf("%d: op %d, %d bytes back, err %v", n, op, len(got), err)
		}
	}
}

func TestDialSendsHeadersAndPath(t *testing.T) {
	var got http.Header
	var uri string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, uri = r.Header.Clone(), r.URL.RequestURI()
		if c, err := Upgrade(w, r, 0); err == nil {
			_ = c.Abort()
		}
	}))
	defer srv.Close()
	h := http.Header{}
	h.Set("Authorization", "Bearer x")
	c, err := Dial(context.Background(), srv.URL+"/v1/a%20b?cmd=sh&cmd=-c", DialOptions{Header: h})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if got.Get("Authorization") != "Bearer x" || uri != "/v1/a%20b?cmd=sh&cmd=-c" {
		t.Fatalf("auth %q uri %q", got.Get("Authorization"), uri)
	}
}

func TestStatusErrorBeforeUpgrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("busy"))
	}))
	defer srv.Close()
	_, err := Dial(context.Background(), srv.URL, DialOptions{})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 || string(se.Body) != "busy" {
		t.Fatalf("got %v", err)
	}
}

func TestRefusedConnectionIsADialError(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	_, err := Dial(context.Background(), "http://"+addr, DialOptions{})
	var de *DialError
	if !errors.As(err, &de) {
		t.Fatalf("got %T %v", err, err)
	}
}

func TestSilentServerIsAHandshakeError(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	_, err := Dial(context.Background(), "http://"+l.Addr().String(), DialOptions{HandshakeTimeout: 200 * time.Millisecond})
	var he *HandshakeError
	if !errors.As(err, &he) {
		t.Fatalf("got %T %v", err, err)
	}
}

func TestCancelDuringHandshakeIsAHandshakeError(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := Dial(ctx, "http://"+l.Addr().String(), DialOptions{})
	var he *HandshakeError
	if !errors.As(err, &he) {
		t.Fatalf("got %T %v", err, err)
	}
}

func TestBadAcceptKeyIsRejected(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: AAAA\r\n\r\n"))
	}()
	_, err := Dial(context.Background(), "http://"+l.Addr().String(), DialOptions{})
	var he *HandshakeError
	if !errors.As(err, &he) {
		t.Fatalf("got %T %v", err, err)
	}
}

func TestPingIsAnsweredAndFragmentsAreJoined(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	pong := make(chan string, 1)
	go func() {
		nc, err := l.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		buf := make([]byte, 4096)
		n, _ := nc.Read(buf)
		var key string
		for _, ln := range strings.Split(string(buf[:n]), "\r\n") {
			if v, ok := strings.CutPrefix(ln, "Sec-WebSocket-Key: "); ok {
				key = v
			}
		}
		_, _ = nc.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"))
		// ping, then a binary message in three fragments with a ping between them
		_, _ = nc.Write([]byte{0x89, 2, 'h', 'i'})
		_, _ = nc.Write([]byte{0x02, 2, 'a', 'b'})
		_, _ = nc.Write([]byte{0x89, 0})
		_, _ = nc.Write([]byte{0x00, 1, 'c'})
		_, _ = nc.Write([]byte{0x80, 1, 'd'})
		// the client's reply: a masked pong carrying "hi", then a masked pong (empty)
		rb := make([]byte, 64)
		m, _ := nc.Read(rb)
		pong <- string(rb[:m])
		_, _ = io.Copy(io.Discard, nc) // stay up until the client is done
	}()
	c, err := Dial(context.Background(), "http://"+l.Addr().String(), DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Abort() }()
	op, p, err := c.ReadMessage()
	if err != nil || op != OpBinary || string(p) != "abcd" {
		t.Fatalf("op %d %q %v", op, p, err)
	}
	select {
	case s := <-pong:
		if len(s) < 2 || s[0] != 0x8A || s[1]&0x80 == 0 {
			t.Fatalf("pong frame % x is not a masked pong", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no pong")
	}
}

func TestServerRejectsUnmaskedClientFrame(t *testing.T) {
	res := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, 0)
		if err != nil {
			return
		}
		defer func() { _ = c.Abort() }()
		_, _, err = c.ReadMessage()
		res <- err
	}))
	defer srv.Close()
	c, err := Dial(context.Background(), srv.URL, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Abort() }()
	if c.c == nil {
		t.Fatal("Dial returned a connection without a socket")
	}
	_, _ = c.c.Write([]byte{0x82, 1, 'x'}) // unmasked, written around the client framing
	select {
	case err := <-res:
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server accepted an unmasked frame")
	}
}

func TestMessageOverTheLimitIsRefused(t *testing.T) {
	srv := echoServer(t, func(c *Conn) { _ = c.WriteMessage(OpBinary, make([]byte, 5000)) })
	c, err := Dial(context.Background(), srv.URL, DialOptions{MaxMessage: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Abort() }()
	if _, _, err := c.ReadMessage(); !errors.Is(err, ErrTooBig) {
		t.Fatalf("got %v", err)
	}
}

func TestCloseFrameEndsTheRead(t *testing.T) {
	srv := echoServer(t, func(c *Conn) { _ = c.Close() })
	c, err := Dial(context.Background(), srv.URL, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Abort() }()
	if _, _, err := c.ReadMessage(); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func TestDroppedSocketIsNotAClose(t *testing.T) {
	srv := echoServer(t, func(c *Conn) { _ = c.Abort() })
	c, err := Dial(context.Background(), srv.URL, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Abort() }()
	_, _, err = c.ReadMessage()
	if err == nil || errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func TestUpgradeRefusesAPlainRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = Upgrade(w, r, 0) }))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
