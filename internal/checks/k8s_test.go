package checks

import (
	"context"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func boolPtr(b bool) *bool { return &b }

func endpointSlice(ns, name, service string, ready ...bool) *discoveryv1.EndpointSlice {
	eps := make([]discoveryv1.Endpoint, len(ready))
	for i, r := range ready {
		eps[i] = discoveryv1.Endpoint{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(r)},
		}
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{endpointSliceServiceLabel: service},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   eps,
	}
}

func TestK8sLister_ReadyCount(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		endpointSlice("prod", "api-abc12", "api", true, true, false),
		endpointSlice("prod", "empty-xyz", "empty", false),
		endpointSlice("staging", "api-abc12", "api", true), // different namespace, must not count
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lister, err := newK8sListerForClientset(ctx, clientset)
	if err != nil {
		t.Fatalf("newK8sListerForClientset: %v", err)
	}

	if got := lister.ReadyCount("prod", "api"); got != 2 {
		t.Errorf("ReadyCount(prod, api) = %d, want 2 (two ready addresses, one not-ready excluded)", got)
	}
	if got := lister.ReadyCount("prod", "empty"); got != 0 {
		t.Errorf("ReadyCount(prod, empty) = %d, want 0", got)
	}
	if got := lister.ReadyCount("staging", "nonexistent"); got != 0 {
		t.Errorf("ReadyCount for unknown service = %d, want 0", got)
	}
	if got := lister.ReadyCount("prod", "nonexistent"); got != 0 {
		t.Errorf("ReadyCount for wrong-namespace match = %d, want 0 (namespace scoping)", got)
	}
}

func TestK8sProber_AgainstFakeInformer(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		endpointSlice("prod", "api-abc12", "api", true),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lister, err := newK8sListerForClientset(ctx, clientset)
	if err != nil {
		t.Fatalf("newK8sListerForClientset: %v", err)
	}

	p := K8sProber{Lister: lister, Namespace: "prod", Service: "api"}
	if !p.Probe(ctx) {
		t.Error("expected up=true against a ready EndpointSlice")
	}

	down := K8sProber{Lister: lister, Namespace: "prod", Service: "missing"}
	if down.Probe(ctx) {
		t.Error("expected up=false for a service with no EndpointSlices")
	}
}
