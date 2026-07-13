package main

import (
	"testing"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

// Bug 3(a) regression: the k8s EndpointSlice lister must only be started
// when the current bundle actually references a k8s-probe localCheck —
// starting it unconditionally at boot was pure overhead (and a needless
// startup failure mode) for the common tcp/http-only bundle.

func TestNeedsK8sProbe_NoRecords(t *testing.T) {
	if needsK8sProbe(nil) {
		t.Fatal("expected false for a nil/empty record set")
	}
	if needsK8sProbe([]bundle.Record{}) {
		t.Fatal("expected false for an empty record set")
	}
}

func TestNeedsK8sProbe_TcpAndHttpOnly(t *testing.T) {
	records := []bundle.Record{
		{
			FQDN: "a.example.com",
			LocalChecks: []bundle.LocalCheck{
				{TargetKey: "k1", Probe: "tcp"},
				{TargetKey: "k2", Probe: "http"},
			},
		},
	}
	if needsK8sProbe(records) {
		t.Fatal("expected false when no localCheck uses probe=k8s")
	}
}

func TestNeedsK8sProbe_FindsK8sProbeAnywhere(t *testing.T) {
	records := []bundle.Record{
		{
			FQDN: "a.example.com",
			LocalChecks: []bundle.LocalCheck{
				{TargetKey: "k1", Probe: "tcp"},
			},
		},
		{
			FQDN: "b.example.com",
			LocalChecks: []bundle.LocalCheck{
				{TargetKey: "k2", Probe: "http"},
				{TargetKey: "k3", Probe: "k8s", Namespace: "prod", Service: "api"},
			},
		},
	}
	if !needsK8sProbe(records) {
		t.Fatal("expected true when any record's localChecks includes probe=k8s, regardless of position")
	}
}

func TestNeedsK8sProbe_NoLocalChecks(t *testing.T) {
	records := []bundle.Record{
		{FQDN: "a.example.com"},
	}
	if needsK8sProbe(records) {
		t.Fatal("expected false for records with no localChecks at all")
	}
}
