// Package tcpproxy is a test TCP forwarder that a test takes down
// (connections refused) and brings back on the same address, to stand in
// for a provider outage.
package tcpproxy

import (
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
)

// Proxy forwards one local address to a target.
type Proxy struct {
	URL    string
	addr   string
	target string
	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	hang   bool // accept and hold connections, never answering
}

// New forwards a fresh local address to endpoint's host (any URL); it
// starts up. URL is endpoint's scheme with the local address.
func New(t testing.TB, endpoint string) *Proxy {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{target: u.Host, conns: map[net.Conn]struct{}{}}
	if p.ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	p.addr = p.ln.Addr().String()
	p.URL = u.Scheme + "://" + p.addr
	go p.serve(p.ln)
	t.Cleanup(p.Down)
	return p
}

// Down closes the listener and every open connection.
func (p *Proxy) Down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	for c := range p.conns {
		_ = c.Close()
		delete(p.conns, c)
	}
}

// Hang accepts connections and never answers, like a black-holed endpoint.
// Up forwards new connections again; held ones stay hung until Down.
func (p *Proxy) Hang(t testing.TB) {
	t.Helper()
	p.Up(t)
	p.mu.Lock()
	p.hang = true
	p.mu.Unlock()
}

// Up listens on the same address again and forwards new connections.
func (p *Proxy) Up(t testing.TB) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hang = false
	if p.ln != nil {
		return
	}
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		t.Fatal(err)
	}
	p.ln = ln
	go p.serve(ln)
}

func (p *Proxy) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.forward(c)
	}
}

func (p *Proxy) forward(c net.Conn) {
	p.mu.Lock()
	hang := p.hang
	if hang {
		p.conns[c] = struct{}{}
	}
	p.mu.Unlock()
	if hang {
		return
	}
	u, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = c.Close()
		return
	}
	if !p.track(c, u) {
		_ = c.Close()
		_ = u.Close()
		return
	}
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go pipe(u, c)
	go pipe(c, u)
	<-done
	_ = c.Close()
	_ = u.Close()
	p.mu.Lock()
	delete(p.conns, c)
	delete(p.conns, u)
	p.mu.Unlock()
}

func (p *Proxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil {
		return false
	}
	for _, c := range conns {
		p.conns[c] = struct{}{}
	}
	return true
}
