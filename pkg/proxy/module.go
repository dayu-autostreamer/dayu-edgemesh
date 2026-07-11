package proxy

import (
	"fmt"

	"k8s.io/klog/v2"

	"github.com/kubeedge/beehive/pkg/core"
	"github.com/kubeedge/edgemesh/pkg/apis/config/defaults"
	"github.com/kubeedge/edgemesh/pkg/apis/config/v1alpha1"
	"github.com/kubeedge/edgemesh/pkg/clients"
	"github.com/kubeedge/edgemesh/pkg/meshstate"
	netutil "github.com/kubeedge/edgemesh/pkg/util/net"
)

// EdgeProxy is used for traffic proxy
type EdgeProxy struct {
	Config      *v1alpha1.EdgeProxyConfig
	ProxyServer *Server
	Socks5Proxy *Socks5Proxy
}

// Name of edgeproxy
func (proxy *EdgeProxy) Name() string {
	return defaults.EdgeProxyModuleName
}

// Group of edgeproxy
func (proxy *EdgeProxy) Group() string {
	return defaults.EdgeProxyModuleName
}

// Enable indicates whether enable this module
func (proxy *EdgeProxy) Enable() bool {
	return proxy.Config.Enable
}

// Start edgeproxy
func (proxy *EdgeProxy) Start() {
	proxy.Run()
}

// Shutdown edgeproxy
func (proxy *EdgeProxy) Shutdown() {
	if proxy.ProxyServer == nil {
		return
	}
	if proxy.ProxyServer.managedRuntime != nil {
		if err := proxy.ProxyServer.managedRuntime.Close(); err != nil {
			klog.ErrorS(err, "Close managed runtime status server")
		}
	}
	err := proxy.ProxyServer.CleanupAndExit()
	if err != nil {
		klog.ErrorS(err, "Cleanup iptables failed")
	}
}

// Register edgeproxy to beehive modules
func Register(c *v1alpha1.EdgeProxyConfig, cli *clients.Clients) error {
	proxy, err := newEdgeProxy(c, cli)
	if err != nil {
		return fmt.Errorf("register module edgeproxy error: %v", err)
	}
	core.Register(proxy)
	return nil
}

func newEdgeProxy(c *v1alpha1.EdgeProxyConfig, cli *clients.Clients) (*EdgeProxy, error) {
	if !c.Enable {
		return &EdgeProxy{Config: c}, nil
	}
	managedRuntime, err := meshstate.NewRuntime(c.ManagedRuntime)
	if err != nil {
		return nil, fmt.Errorf("initialize managed runtime: %w", err)
	}
	// get proxy listen ip
	listenIP, err := netutil.GetInterfaceIP(c.ListenInterface)
	if err != nil {
		return nil, fmt.Errorf("get proxy listen ip err: %v", err)
	}

	// new proxy server
	proxyServer, err := newProxyServer(NewDefaultKubeProxyConfiguration(listenIP.String()), c.LoadBalancer, cli.GetKubeClient(), cli.GetIstioClient(), c.ServiceFilterMode, managedRuntime)
	if err != nil {
		return nil, fmt.Errorf("new proxy server err: %v", err)
	}

	// new socks5 proxy
	var socks5Proxy *Socks5Proxy
	if c.Socks5Proxy.Enable {
		socks5Proxy, err = NewSocks5Proxy(c.Socks5Proxy, listenIP, cli.GetKubeClient())
		if err != nil {
			return nil, fmt.Errorf("new socks5Proxy err: %w", err)
		}
	}

	return &EdgeProxy{
		Config:      c,
		ProxyServer: proxyServer,
		Socks5Proxy: socks5Proxy,
	}, nil
}
