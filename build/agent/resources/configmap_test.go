package resources_test

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type configMap struct {
	Data map[string]string `json:"data"`
}

type managedRuntimeConfig struct {
	Enable bool `json:"enable"`
}

type loadBalancerConfig struct {
	ConsistentHash *struct {
		PartitionCount    int     `json:"partitionCount"`
		ReplicationFactor int     `json:"replicationFactor"`
		Load              float64 `json:"load"`
	} `json:"consistentHash"`
}

type agentConfig struct {
	Modules struct {
		EdgeProxy struct {
			ServiceFilterMode string                `json:"serviceFilterMode"`
			ManagedRuntime    *managedRuntimeConfig `json:"managedRuntime"`
			LoadBalancer      *loadBalancerConfig   `json:"loadBalancer"`
		} `json:"edgeProxy"`
		ManagedRuntime *struct{} `json:"managedRuntime"`
		LoadBalancer   *struct{} `json:"loadBalancer"`
	} `json:"modules"`
}

type helmValues struct {
	Agent struct {
		Modules struct {
			EdgeProxy struct {
				ServiceFilterMode string `json:"serviceFilterMode"`
				ManagedRuntime    struct {
					Enable bool   `json:"enable"`
					Image  string `json:"image"`
				} `json:"managedRuntime"`
			} `json:"edgeProxy"`
		} `json:"modules"`
	} `json:"agent"`
}

func TestRawAgentConfigKeepsEdgeProxyOptionsNested(t *testing.T) {
	raw, err := os.ReadFile("04-configmap.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var manifest configMap
	if err := yaml.Unmarshal([]byte(strings.SplitN(string(raw), "\n---", 2)[0]), &manifest); err != nil {
		t.Fatalf("parse agent ConfigMap: %v", err)
	}

	var config agentConfig
	if err := yaml.Unmarshal([]byte(manifest.Data["edgemesh-agent.yaml"]), &config); err != nil {
		t.Fatalf("parse embedded agent config: %v", err)
	}

	edgeProxy := config.Modules.EdgeProxy
	if edgeProxy.ServiceFilterMode != "FilterIfLabelExists" {
		t.Fatalf("serviceFilterMode = %q, want FilterIfLabelExists", edgeProxy.ServiceFilterMode)
	}
	if edgeProxy.ManagedRuntime == nil || !edgeProxy.ManagedRuntime.Enable {
		t.Fatalf("managedRuntime = %#v, want nested enable=true", edgeProxy.ManagedRuntime)
	}
	if edgeProxy.LoadBalancer == nil || edgeProxy.LoadBalancer.ConsistentHash == nil {
		t.Fatalf("loadBalancer = %#v, want nested consistentHash settings", edgeProxy.LoadBalancer)
	}
	if config.Modules.ManagedRuntime != nil || config.Modules.LoadBalancer != nil {
		t.Fatal("managedRuntime and loadBalancer must be nested under modules.edgeProxy")
	}
}

func TestHelmDefaultsEnableManagedRuntimeWithMatchingImage(t *testing.T) {
	raw, err := os.ReadFile("../../helm/edgemesh/values.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var values helmValues
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse Helm values: %v", err)
	}

	edgeProxy := values.Agent.Modules.EdgeProxy
	if edgeProxy.ServiceFilterMode != "FilterIfLabelExists" {
		t.Fatalf("serviceFilterMode = %q, want FilterIfLabelExists", edgeProxy.ServiceFilterMode)
	}
	if !edgeProxy.ManagedRuntime.Enable {
		t.Fatal("Helm defaults must enable managed runtime")
	}
	if edgeProxy.ManagedRuntime.Image != "dayuhub/edgemesh-agent:v1.1" {
		t.Fatalf("managed runtime image = %q, want dayuhub/edgemesh-agent:v1.1", edgeProxy.ManagedRuntime.Image)
	}
}
