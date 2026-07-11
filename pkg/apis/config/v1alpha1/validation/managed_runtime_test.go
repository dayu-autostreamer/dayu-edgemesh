package validation

import (
	"testing"

	"github.com/kubeedge/edgemesh/pkg/apis/config/defaults"
	"github.com/kubeedge/edgemesh/pkg/apis/config/v1alpha1"
)

func TestValidateManagedRuntimeFeatureGate(t *testing.T) {
	tests := []struct {
		name      string
		config    *v1alpha1.EdgeProxyConfig
		wantError bool
	}{
		{
			name: "legacy config omits managed runtime block",
			config: &v1alpha1.EdgeProxyConfig{
				Enable:            true,
				ServiceFilterMode: defaults.FilterIfLabelDoesNotExistsMode,
			},
		},
		{
			name: "disabled preserves legacy filter mode",
			config: &v1alpha1.EdgeProxyConfig{
				Enable:            true,
				ServiceFilterMode: defaults.FilterIfLabelDoesNotExistsMode,
				ManagedRuntime:    &v1alpha1.ManagedRuntimeConfig{Enable: false},
			},
		},
		{
			name: "enabled with primary informer mode",
			config: &v1alpha1.EdgeProxyConfig{
				Enable:            true,
				ServiceFilterMode: defaults.FilterIfLabelExistsMode,
				ManagedRuntime:    &v1alpha1.ManagedRuntimeConfig{Enable: true},
			},
		},
		{
			name: "enabled requires edge proxy",
			config: &v1alpha1.EdgeProxyConfig{
				Enable:            false,
				ServiceFilterMode: defaults.FilterIfLabelExistsMode,
				ManagedRuntime:    &v1alpha1.ManagedRuntimeConfig{Enable: true},
			},
			wantError: true,
		},
		{
			name: "enabled requires shared primary informer mode",
			config: &v1alpha1.EdgeProxyConfig{
				Enable:            true,
				ServiceFilterMode: defaults.FilterIfLabelDoesNotExistsMode,
				ManagedRuntime:    &v1alpha1.ManagedRuntimeConfig{Enable: true},
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateModuleEdgeProxy(tt.config)
			if got := len(errs) != 0; got != tt.wantError {
				t.Fatalf("ValidateModuleEdgeProxy() errors=%v, wantError=%v", errs, tt.wantError)
			}
		})
	}
}

func TestDefaultConfigurationKeepsManagedRuntimeDisabled(t *testing.T) {
	config := v1alpha1.NewDefaultEdgeMeshAgentConfig("/tmp/edgemesh-agent.yaml")
	managed := config.Modules.EdgeProxyConfig.ManagedRuntime
	if managed == nil || managed.Enable {
		t.Fatalf("default managedRuntime = %#v, want explicit enable=false", managed)
	}
}
