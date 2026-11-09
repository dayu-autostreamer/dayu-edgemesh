package userspace

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/flynn/go-shlex"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/util/iptables"
	netutils "k8s.io/utils/net"
)

// portalRuleKey ignores option ordering and host CIDR formatting introduced by
// iptables-save. Values and negated matches remain part of the rule identity.
func portalRuleKey(args []string) string {
	clauses := []string{}
	for i := 0; i < len(args); i++ {
		negation := ""
		if args[i] == "!" {
			negation = "!"
			i++
		}
		if i == len(args) {
			break
		}
		option := args[i]
		value := ""
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && args[i+1] != "!" {
			i++
			value = args[i]
			if option == "-d" || option == "-s" {
				value = strings.TrimSuffix(strings.TrimSuffix(value, "/32"), "/128")
			}
		}
		clauses = append(clauses, negation+option+"="+value)
	}
	sort.Strings(clauses)
	return strings.Join(clauses, "\x00")
}

// Called with proxier.mu held, after initial Service/Endpoints synchronization.
// Only our dedicated chains are reconciled; the rest of the host is untouched.
func (proxier *Proxier) cleanupOrphanedRules() error {
	wanted := map[iptables.Table]map[iptables.Chain]map[string]bool{
		iptables.TableNAT: {
			iptablesContainerPortalChain: {}, iptablesHostPortalChain: {},
			iptablesContainerNodePortChain: {}, iptablesHostNodePortChain: {},
		},
		iptables.TableFilter: {iptablesNonLocalNodePortChain: {}},
	}
	add := func(table iptables.Table, chain iptables.Chain, args []string) {
		wanted[table][chain][portalRuleKey(args)] = true
	}
	addPortal := func(p portal, name proxy.ServicePortName, info *ServiceInfo) {
		add(iptables.TableNAT, iptablesContainerPortalChain,
			proxier.iptablesContainerPortalArgs(p.ip, p.isExternal, false, p.port, info.protocol, proxier.listenIP, info.proxyPort, name))
		if p.isExternal {
			add(iptables.TableNAT, iptablesContainerPortalChain,
				proxier.iptablesContainerPortalArgs(p.ip, false, true, p.port, info.protocol, proxier.listenIP, info.proxyPort, name))
		}
		add(iptables.TableNAT, iptablesHostPortalChain,
			proxier.iptablesHostPortalArgs(p.ip, p.isExternal, p.port, info.protocol, proxier.listenIP, info.proxyPort, name))
	}
	for name, info := range proxier.serviceMap {
		if info.teardownPending {
			continue
		}
		addPortal(info.portal, name, info)
		for _, ip := range info.externalIPs {
			addPortal(portal{netutils.ParseIPSloppy(ip), info.portal.port, true}, name, info)
		}
		for _, ingress := range info.loadBalancerStatus.Ingress {
			if ingress.IP != "" {
				addPortal(portal{netutils.ParseIPSloppy(ingress.IP), info.portal.port, false}, name, info)
			}
		}
		if info.nodePort != 0 {
			add(iptables.TableNAT, iptablesContainerNodePortChain,
				proxier.iptablesContainerPortalArgs(nil, false, false, info.nodePort, info.protocol, proxier.listenIP, info.proxyPort, name))
			add(iptables.TableNAT, iptablesHostNodePortChain,
				proxier.iptablesHostNodePortArgs(info.nodePort, info.protocol, proxier.listenIP, info.proxyPort, name))
			add(iptables.TableFilter, iptablesNonLocalNodePortChain,
				proxier.iptablesNonLocalNodePortArgs(info.nodePort, info.protocol, proxier.listenIP, info.proxyPort, name))
		}
	}
	for _, table := range []iptables.Table{iptables.TableNAT, iptables.TableFilter} {
		var dump bytes.Buffer
		if err := proxier.iptables.SaveInto(table, &dump); err != nil {
			return err
		}
		for _, line := range strings.Split(dump.String(), "\n") {
			if !strings.HasPrefix(line, "-A ") {
				continue
			}
			args, err := shlex.Split(line)
			if err != nil {
				return fmt.Errorf("parse %s rules: %w", table, err)
			}
			if len(args) < 3 {
				continue
			}
			chain := iptables.Chain(args[1])
			expected, owned := wanted[table][chain]
			if !owned || expected[portalRuleKey(args[2:])] {
				continue
			}
			if err := proxier.iptables.DeleteRule(table, chain, args[2:]...); err != nil {
				return err
			}
		}
	}
	return nil
}
