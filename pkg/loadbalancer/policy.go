package loadbalancer

import (
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sync"

	istioapi "istio.io/client-go/pkg/apis/networking/v1alpha3"
    "k8s.io/klog/v2"
	"github.com/kubeedge/edgemesh/pkg/apis/config/v1alpha1"
)

const (
	RoundRobin     = "ROUND_ROBIN"
	Random         = "RANDOM"
	ConsistentHash = "CONSISTENT_HASH"

	HTTPHeader   = "HTTP_HEADER"
	UserSourceIP = "USER_SOURCE_IP"
)

type Policy interface {
	Name() string
	Update(oldDr, dr *istioapi.DestinationRule)
	Pick(endpoints []string, srcAddr net.Addr, tcpConn net.Conn, cliReq *http.Request) (string, *http.Request, error)
	Sync(endpoints []string)
	Release()
}

// RoundRobinPolicy is a default policy.
type RoundRobinPolicy struct {
}

func NewRoundRobinPolicy() *RoundRobinPolicy {
	return &RoundRobinPolicy{}
}

func (*RoundRobinPolicy) Name() string {
	return RoundRobin
}

func (*RoundRobinPolicy) Update(_, _ *istioapi.DestinationRule) {}

func (*RoundRobinPolicy) Pick(_ []string, _ net.Addr, _ net.Conn, cliReq *http.Request) (string, *http.Request, error) {
	// RoundRobinPolicy is an empty implementation and we won't use it,
	// the outer round-robin policy will be used next.
	return "", cliReq, fmt.Errorf("call RoundRobinPolicy is forbidden")
}

func (*RoundRobinPolicy) Sync(_ []string) {}

func (*RoundRobinPolicy) Release() {}

type RandomPolicy struct {
	lock sync.Mutex
}

func NewRandomPolicy() *RandomPolicy {
	return &RandomPolicy{}
}

func (rd *RandomPolicy) Name() string {
	return Random
}

func (rd *RandomPolicy) Update(_, _ *istioapi.DestinationRule) {}

func (rd *RandomPolicy) Pick(endpoints []string, _ net.Addr, _ net.Conn, cliReq *http.Request) (string, *http.Request, error) {
	rd.lock.Lock()
	k := rand.Int() % len(endpoints)
	rd.lock.Unlock()
	return endpoints[k], cliReq, nil
}

func (rd *RandomPolicy) Sync(_ []string) {}

func (rd *RandomPolicy) Release() {}

type ConsistentHashPolicy struct {
	Config     *v1alpha1.ConsistentHash
	lock       sync.Mutex
	endpoints  []string          // Store the list of endpoints
	hashKey    HashKey           // Hash key for selecting the target node
}

func NewConsistentHashPolicy(config *v1alpha1.ConsistentHash, dr *istioapi.DestinationRule, endpoints []string) *ConsistentHashPolicy {
	klog.Infof("[New no balance version] create a no balance policy")
	return &ConsistentHashPolicy{
		Config:    config,
		endpoints: endpoints,
		hashKey:   getConsistentHashKey(dr),
	}
}

func (ch *ConsistentHashPolicy) Name() string {
	return ConsistentHash
}

func (ch *ConsistentHashPolicy) Update(_, dr *istioapi.DestinationRule) {
	ch.lock.Lock()
	ch.hashKey = getConsistentHashKey(dr)
	ch.lock.Unlock()
}

func (ch *ConsistentHashPolicy) Pick(_ []string, srcAddr net.Addr, netConn net.Conn, cliReq *http.Request) (endpoint string, req *http.Request, err error) {

	ch.lock.Lock()
	defer ch.lock.Unlock()
	req = cliReq
    klog.Infof("[New no balance version] no balance policy pick")
	// Extract the target IP from the request's destination address
	var targetIP string
	if req != nil && req.URL != nil {
		// Extract IP address from request's Host or URL
		targetIP = req.URL.Hostname() // Extract the hostname part as the target IP
	} else if netConn != nil {
		// Fall back to extracting IP from the network connection if available
		targetIP, _, _ = net.SplitHostPort(netConn.RemoteAddr().String())
	}

	if targetIP == "" {
		return "", req, fmt.Errorf("unable to determine target IP from request")
	}

	// Match the target IP with endpoints to locate the correct node
	for _, endpoint := range ch.endpoints {
		endpointIP, _, err := net.SplitHostPort(endpoint)
		if err != nil {
			continue // Skip invalid endpoint formats
		}
		if endpointIP == targetIP {
			return endpoint, req, nil // Return the matched endpoint directly
		}
	}

	// If no matching endpoint is found, return an error
	return "", req, fmt.Errorf("no matching node found for target IP: %s", targetIP)
}

func (ch *ConsistentHashPolicy) Sync(endpoints []string) {
	ch.lock.Lock()
	ch.endpoints = endpoints
	ch.lock.Unlock()
}

func (ch *ConsistentHashPolicy) Release() {
	ch.lock.Lock()
	ch.endpoints = nil // Clear the endpoint list
	ch.lock.Unlock()
}