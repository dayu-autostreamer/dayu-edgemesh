package proxy

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlowSetupDoesNotBlockNextTCPConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	socket := &tcpProxySocket{Listener: listener}
	first, second, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	var calls int32
	go func() {
		socket.proxyConnections(func() bool { return true }, func(net.Conn) (net.Conn, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				close(first)
				<-release
			} else {
				close(second)
			}
			return nil, errors.New("injected dial failure")
		})
		close(done)
	}()
	a, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first setup did not start")
	}
	b, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("slow first setup blocked Accept")
	}
	listener.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closing listener did not stop Accept")
	}
}

func TestTCPSetupConcurrencyIsBounded(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	socket := &tcpProxySocket{Listener: listener}
	release := make(chan struct{})
	defer close(release)
	var active, peak int32
	go socket.proxyConnections(func() bool { return true }, func(net.Conn) (net.Conn, error) {
		current := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if current <= old || atomic.CompareAndSwapInt32(&peak, old, current) {
				break
			}
		}
		<-release
		atomic.AddInt32(&active, -1)
		return nil, errors.New("injected failure")
	})
	for i := 0; i < 40; i++ {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
	}
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&peak) != 32 {
		t.Fatalf("setup peak=%d, want 32", atomic.LoadInt32(&peak))
	}
}
