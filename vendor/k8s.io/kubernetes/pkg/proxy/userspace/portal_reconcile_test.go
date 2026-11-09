package userspace

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/util/iptables"
)

func TestKernelReconcileRemovesOrphansAndKeepsActivePortals(t *testing.T) {
	ipt := newFakeIPTables()
	p := &Proxier{iptables: ipt, listenIP: net.ParseIP("169.254.96.16"), serviceMap: map[proxy.ServicePortName]*ServiceInfo{}}
	name := proxy.ServicePortName{NamespacedName: types.NamespacedName{Namespace: "active", Name: "redis"}, Port: "tcp"}
	info := &ServiceInfo{portal: portal{ip: net.ParseIP("10.96.0.12"), port: 6379}, protocol: v1.ProtocolTCP, proxyPort: 40000, nodePort: 30000}
	p.serviceMap[name] = info
	active := p.iptablesContainerPortalArgs(info.portal.ip, false, false, 6379, info.protocol, p.listenIP, info.proxyPort, name)
	ipt.EnsureRule(iptables.Append, iptables.TableNAT, iptablesContainerPortalChain, active...)
	old := append([]string(nil), active...)
	old[len(old)-1] = "169.254.96.16:39999"
	ipt.EnsureRule(iptables.Append, iptables.TableNAT, iptablesContainerPortalChain, old...)
	staleName := name
	staleName.Namespace = "deleted"
	orphan := p.iptablesHostNodePortArgs(30000, info.protocol, p.listenIP, 39998, staleName)
	ipt.EnsureRule(iptables.Append, iptables.TableNAT, iptablesHostNodePortChain, orphan...)
	replacement := p.iptablesHostNodePortArgs(30000, info.protocol, p.listenIP, info.proxyPort, name)
	ipt.EnsureRule(iptables.Append, iptables.TableNAT, iptablesHostNodePortChain, replacement...)
	filter := p.iptablesNonLocalNodePortArgs(30000, info.protocol, p.listenIP, 39998, staleName)
	ipt.EnsureRule(iptables.Append, iptables.TableFilter, iptablesNonLocalNodePortChain, filter...)
	ipt.EnsureRule(iptables.Append, iptables.TableNAT, "OTHER-OWNER", orphan...)
	if err := p.cleanupOrphanedRules(); err != nil {
		t.Fatal(err)
	}
	if len(ipt.rules[iptables.TableNAT][iptablesContainerPortalChain]) != 1 {
		t.Fatal("stale same-name portal remains")
	}
	if _, ok := ipt.rules[iptables.TableNAT][iptablesContainerPortalChain][ruleKey(active)]; !ok {
		t.Fatal("active portal deleted")
	}
	if len(ipt.rules[iptables.TableNAT][iptablesHostNodePortChain]) != 1 {
		t.Fatal("stale NodePort remains")
	}
	if len(ipt.rules[iptables.TableFilter][iptablesNonLocalNodePortChain]) != 0 {
		t.Fatal("orphan filter rule remains")
	}
	if len(ipt.rules[iptables.TableNAT]["OTHER-OWNER"]) != 1 {
		t.Fatal("another owner's chain was changed")
	}
	if err := p.cleanupOrphanedRules(); err != nil {
		t.Fatal(err)
	}
}

func TestPortalRuleIdentitySurvivesIPTablesSaveFormatting(t *testing.T) {
	expected := strings.Fields("-m comment --comment ns/svc:tcp -p tcp -m tcp --dport 80 -d 10.96.0.4 -m physdev ! --physdev-is-in -j DNAT --to-destination 169.254.96.16:40000")
	actual := strings.Fields("-d 10.96.0.4/32 -p tcp -m comment --comment ns/svc:tcp -m tcp --dport 80 -m physdev ! --physdev-is-in -j DNAT --to-destination 169.254.96.16:40000")
	if portalRuleKey(expected) != portalRuleKey(actual) {
		t.Fatal("save formatting changed identity")
	}
	actual[len(actual)-1] = "169.254.96.16:40001"
	if portalRuleKey(expected) == portalRuleKey(actual) {
		t.Fatal("different proxy socket considered active")
	}
}

type failingReconcileRules struct {
	*fakeIPTables
	failure string
}

func (ipt *failingReconcileRules) SaveInto(table iptables.Table, buffer *bytes.Buffer) error {
	if ipt.failure == "save" {
		return errors.New("injected iptables-save failure")
	}
	return ipt.fakeIPTables.SaveInto(table, buffer)
}

func (ipt *failingReconcileRules) EnsureChain(table iptables.Table, chain iptables.Chain) (bool, error) {
	if ipt.failure == "init" {
		return false, errors.New("injected chain initialization failure")
	}
	return ipt.fakeIPTables.EnsureChain(table, chain)
}

func (ipt *failingReconcileRules) DeleteRule(table iptables.Table, chain iptables.Chain, args ...string) error {
	if ipt.failure == "delete" {
		return errors.New("injected rule deletion failure")
	}
	return ipt.fakeIPTables.DeleteRule(table, chain, args...)
}

func TestOrphanCleanupFailureDoesNotAcknowledgeShadowedNewRoute(t *testing.T) {
	for _, failure := range []string{"init", "save", "delete"} {
		t.Run(failure, func(t *testing.T) {
			ipt := &failingReconcileRules{fakeIPTables: newFakeIPTables()}
			p, err := createProxier(&fakeLoadBalancer{}, net.ParseIP("169.254.20.10"), ipt, nil,
				net.ParseIP("169.254.20.10"), newPortAllocator(utilnet.PortRange{}),
				time.Second, time.Second, time.Second, newFakeProxySocket)
			if err != nil {
				t.Fatal(err)
			}
			defer p.shutdown()
			service := testService("sample", "redis", "tcp", "10.96.0.12", 6379, 0)
			service.UID = "new"
			name := proxy.ServicePortName{NamespacedName: types.NamespacedName{Namespace: service.Namespace, Name: service.Name}, Port: "tcp"}
			old := p.iptablesContainerPortalArgs(net.ParseIP(service.Spec.ClusterIP), false, false, 6379, v1.ProtocolTCP, p.listenIP, 39999, name)
			ipt.EnsureRule(iptables.Append, iptables.TableNAT, iptablesContainerPortalChain, old...)
			ipt.failure = failure
			applied := false
			p.SetManagedServiceHandler(func(*v1.Service) bool { return true })
			p.SetServiceAppliedHandler(func(_ proxy.ServicePortName, _ types.UID, ready bool) { applied = ready })
			p.OnServiceAdd(service)
			atomic.StoreInt32(&p.initialized, 1)
			p.syncProxyRules()
			if applied || len(p.serviceMap) != 0 || len(p.serviceChanges) != 1 {
				t.Fatal("unreconciled old rule allowed a new route to be acknowledged or its update was lost")
			}
			ipt.failure = ""
			p.syncProxyRules()
			if !applied || len(p.serviceMap) != 1 || len(p.serviceChanges) != 0 {
				t.Fatal("route did not recover through the original pending update")
			}
			if _, exists := ipt.rules[iptables.TableNAT][iptablesContainerPortalChain][ruleKey(old)]; exists {
				t.Fatal("old rule still shadows the new route")
			}
		})
	}
}
