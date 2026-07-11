package proxy

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKubernetesEndpointHostPrefersHTTPSPort(t *testing.T) {
	host, err := kubernetesEndpointHost(&v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "kubernetes",
		},
		Subsets: []v1.EndpointSubset{{
			Addresses: []v1.EndpointAddress{{IP: "10.0.0.2"}},
			Ports: []v1.EndpointPort{
				{Name: "metrics", Port: 4443, Protocol: v1.ProtocolTCP},
				{Name: "https", Port: 6443, Protocol: v1.ProtocolTCP},
			},
		}},
	})
	if err != nil {
		t.Fatalf("kubernetesEndpointHost returned error: %v", err)
	}
	if host != "https://10.0.0.2:6443" {
		t.Fatalf("expected https endpoint host, got %q", host)
	}
}

func TestKubernetesEndpointHostRequiresReadyTCPAddress(t *testing.T) {
	if _, err := kubernetesEndpointHost(&v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "kubernetes",
		},
		Subsets: []v1.EndpointSubset{{
			NotReadyAddresses: []v1.EndpointAddress{{IP: "10.0.0.2"}},
			Ports:             []v1.EndpointPort{{Name: "https", Port: 6443, Protocol: v1.ProtocolTCP}},
		}},
	}); err == nil {
		t.Fatalf("expected kubernetesEndpointHost to fail when no ready address exists")
	}
}

func TestIsMetaServerHost(t *testing.T) {
	testCases := []struct {
		host string
		want bool
	}{
		{host: "http://127.0.0.1:10550", want: true},
		{host: "https://localhost:10550", want: true},
		{host: "https://[::1]:10550", want: true},
		{host: "https://127.0.0.1:6443", want: false},
		{host: "https://10.0.0.2:6443", want: false},
		{host: "", want: false},
	}

	for _, tc := range testCases {
		if got := isMetaServerHost(tc.host); got != tc.want {
			t.Fatalf("isMetaServerHost(%q) = %t, want %t", tc.host, got, tc.want)
		}
	}
}
