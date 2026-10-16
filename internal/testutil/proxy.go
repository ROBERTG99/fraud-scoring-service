package testutil

import (
	"io"
	"net"
	"sync"
	"testing"
)

// Proxy is a TCP proxy that a test can cut (close live connections, refuse new ones) and restore,
// to simulate a dependency outage without stopping the real server.
type Proxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	down   bool
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

// NewProxy listens on 127.0.0.1:0 and forwards to target until the test ends.
func NewProxy(t testing.TB, target string) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.Cut()
		p.wg.Wait()
	})
	return p
}

// Addr is the proxy's host:port.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Cut closes every live connection and refuses new ones.
func (p *Proxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

// Restore accepts connections again.
func (p *Proxy) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = false
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) serve() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		if !p.track(client) {
			_ = client.Close()
			continue
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil || !p.track(server) {
			_ = client.Close()
			if server != nil {
				_ = server.Close()
			}
			continue
		}
		p.wg.Add(2)
		go p.pipe(client, server)
		go p.pipe(server, client)
	}
}

func (p *Proxy) pipe(dst, src net.Conn) {
	defer p.wg.Done()
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}
