package critical_test

import (
	"io"
	"net"
	"sync"
	"testing"
)

// tcpCut forwards real protocol bytes. Cutting it disconnects the owned
// provider connections; it never substitutes an application response.
type tcpCut struct {
	listener    net.Listener
	mu          sync.Mutex
	closed      bool
	connections map[net.Conn]bool
}

func newTCPCut(t testing.TB, upstream string) *tcpCut {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	p := &tcpCut{listener: listener, connections: map[net.Conn]bool{}}
	t.Cleanup(p.cut)
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				client.Close()
				continue
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				client.Close()
				server.Close()
				continue
			}
			p.connections[client], p.connections[server] = true, true
			p.mu.Unlock()
			go func() {
				defer func() {
					client.Close()
					server.Close()
					p.mu.Lock()
					delete(p.connections, client)
					delete(p.connections, server)
					p.mu.Unlock()
				}()
				go func() { _, _ = io.Copy(server, client); server.Close() }()
				_, _ = io.Copy(client, server)
			}()
		}
	}()
	return p
}

func (p *tcpCut) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.listener.Close()
	for c := range p.connections {
		c.Close()
	}
}
