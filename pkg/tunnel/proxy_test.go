package tunnel

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	proxypb "github.com/kubeedge/edgemesh/pkg/tunnel/pb/proxy"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	"github.com/libp2p/go-msgio/protoio"
	ma "github.com/multiformats/go-multiaddr"
)

type pipeStream struct {
	network.Stream
	connection net.Conn
	resets     int32
}

func (s *pipeStream) Read(p []byte) (int, error)         { return s.connection.Read(p) }
func (s *pipeStream) Write(p []byte) (int, error)        { return s.connection.Write(p) }
func (s *pipeStream) Close() error                       { return s.connection.Close() }
func (s *pipeStream) Reset() error                       { atomic.AddInt32(&s.resets, 1); return s.connection.Close() }
func (s *pipeStream) SetDeadline(d time.Time) error      { return s.connection.SetDeadline(d) }
func (s *pipeStream) SetReadDeadline(d time.Time) error  { return s.connection.SetReadDeadline(d) }
func (s *pipeStream) SetWriteDeadline(d time.Time) error { return s.connection.SetWriteDeadline(d) }
func (s *pipeStream) Protocol() protocol.ID              { return "test" }
func (s *pipeStream) Conn() network.Conn                 { return pipeNetworkConn{} }

type pipeNetworkConn struct{ network.Conn }

func (pipeNetworkConn) LocalMultiaddr() ma.Multiaddr  { return ma.StringCast("/ip4/127.0.0.1/tcp/1") }
func (pipeNetworkConn) RemoteMultiaddr() ma.Multiaddr { return ma.StringCast("/ip4/127.0.0.1/tcp/2") }

type pipeHost struct {
	host.Host
	stream network.Stream
	store  peerstore.Peerstore
}

func (h pipeHost) NewStream(ctx context.Context, p peer.ID, ids ...protocol.ID) (network.Stream, error) {
	return h.stream, nil
}
func (h pipeHost) Peerstore() peerstore.Peerstore { return h.store }

func TestProxyHandshakeTimeoutResetsStream(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	stream := &pipeStream{connection: a}
	store, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tunnel := &EdgeTunnel{p2pHost: pipeHost{stream: stream, store: store}, nodePeerMap: map[string]peer.ID{"edge": peer.ID("peer")}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := tunnel.GetProxyStream(ctx, ProxyOptions{NodeName: "edge", Protocol: TCP, IP: "127.0.0.1", Port: 1}); err == nil {
		t.Fatal("silent peer accepted")
	}
	if time.Since(started) > time.Second || atomic.LoadInt32(&stream.resets) != 1 {
		t.Fatal("handshake was not bounded and reset")
	}
}

func TestSuccessfulProxyStreamClearsHandshakeDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	stream := &pipeStream{connection: a}
	store, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tunnel := &EdgeTunnel{p2pHost: pipeHost{stream: stream, store: store}, nodePeerMap: map[string]peer.ID{"edge": peer.ID("peer")}}
	go func() {
		msg := new(proxypb.Proxy)
		if protoio.NewDelimitedReader(b, MaxReadSize).ReadMsg(msg) != nil {
			return
		}
		msg.Type = proxypb.Proxy_SUCCESS.Enum()
		if protoio.NewDelimitedWriter(b).WriteMsg(msg) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
		_, _ = b.Write([]byte("x"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	conn, err := tunnel.GetProxyStream(ctx, ProxyOptions{NodeName: "edge", Protocol: TCP, IP: "127.0.0.1", Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data := make([]byte, 1)
	if _, err := conn.Read(data); err != nil || data[0] != 'x' {
		t.Fatalf("established stream retained setup deadline: %v", err)
	}
	if atomic.LoadInt32(&stream.resets) != 0 {
		t.Fatal("successful stream reset")
	}
}

func TestPeerDialHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if conn, err := tryDialEndpoint(ctx, TCP, "127.0.0.1", 1); err == nil {
		conn.Close()
		t.Fatal("cancelled dial succeeded")
	}
}

func TestPeerCacheAndRelaySnapshotAreIndependent(t *testing.T) {
	tunnel := &EdgeTunnel{nodePeerMap: map[string]peer.ID{}, relayMap: RelayMap{"edge": {ID: peer.ID("peer"), Addrs: []ma.Multiaddr{ma.StringCast("/ip4/127.0.0.1/tcp/1")}}}}
	tunnel.rememberPeer("edge", peer.ID("peer"))
	snapshot := tunnel.relaySnapshot()
	snapshot["edge"].Addrs[0] = ma.StringCast("/ip4/127.0.0.1/tcp/2")
	if tunnel.relaySnapshot()["edge"].Addrs[0].String() != "/ip4/127.0.0.1/tcp/1" {
		t.Fatal("relay snapshot aliases mutable state")
	}
	if id, ok := tunnel.cachedPeer("edge"); !ok || id != peer.ID("peer") {
		t.Fatal("peer cache lost mapping")
	}
}

func TestFailedProxyHandshakeResetsStreamOnce(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	stream := &pipeStream{connection: a}
	store, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tunnel := &EdgeTunnel{p2pHost: pipeHost{stream: stream, store: store}, nodePeerMap: map[string]peer.ID{"edge": peer.ID("peer")}}
	go func() {
		msg := new(proxypb.Proxy)
		if protoio.NewDelimitedReader(b, MaxReadSize).ReadMsg(msg) == nil {
			msg.Type = proxypb.Proxy_FAILED.Enum()
			_ = protoio.NewDelimitedWriter(b).WriteMsg(msg)
		}
	}()
	if _, err := tunnel.GetProxyStream(context.Background(), ProxyOptions{NodeName: "edge", Protocol: TCP, IP: "127.0.0.1", Port: 1}); err == nil {
		t.Fatal("failed handshake accepted")
	}
	if atomic.LoadInt32(&stream.resets) != 1 {
		t.Fatal("failed stream must be reset exactly once")
	}
}

func TestReceivingProxyHandshakeHonorsDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	stream := &pipeStream{connection: a}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	tunnel := &EdgeTunnel{hostCtx: ctx}
	started := time.Now()
	tunnel.proxyStreamHandler(stream)
	if time.Since(started) > time.Second || atomic.LoadInt32(&stream.resets) != 1 {
		t.Fatal("silent client kept receiving handshake alive")
	}
}

func TestConcurrentPeerDiscoveryAndProxyLookup(t *testing.T) {
	tunnel := &EdgeTunnel{nodePeerMap: map[string]peer.ID{}, relayMap: RelayMap{}}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				tunnel.rememberPeer("edge", peer.ID("peer"))
				tunnel.cachedPeer("edge")
				tunnel.nodeNameFromPeerID(peer.ID("peer"))
				tunnel.refreshRelayMap("edge", &peer.AddrInfo{ID: peer.ID("peer"), Addrs: []ma.Multiaddr{ma.StringCast("/ip4/8.8.8.8/tcp/1")}})
				tunnel.relaySnapshot()
			}
		}()
	}
	workers.Wait()
}
