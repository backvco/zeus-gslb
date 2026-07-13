package dnsserver

import (
	"testing"

	"github.com/miekg/dns"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

// fixedStore implements Store with a bundle fixed at construction, so tests
// don't need bundle.Store's file/polling machinery.
type fixedStore struct{ b *bundle.Bundle }

func (f fixedStore) Current() *bundle.Bundle { return f.b }

func testBundle() *bundle.Bundle {
	return &bundle.Bundle{
		ProtocolVersion: 1,
		Domain:          "z-backv.local",
		GeneratedAt:     "2026-07-10T00:00:00.000Z",
		ContentHash:     "deadbeef",
		Records: []bundle.Record{
			{
				FQDN:  "mysql-01-rw.prod.app1.z-backv.local",
				TTL:   7,
				State: bundle.StateOK,
				Answers: []bundle.Answer{
					{IP: "10.0.5.5", Port: 443},
					{IP: "10.0.5.6", Port: 443},
					{IP: "10.0.5.7", Port: 443},
				},
			},
			{
				FQDN:  "shared-cache.app1.z-backv.local",
				TTL:   5,
				State: bundle.StateDegraded,
				Answers: []bundle.Answer{
					{IP: "10.0.9.9", Port: 6379},
				},
			},
			{
				FQDN:    "down-service.app1.z-backv.local",
				TTL:     5,
				State:   bundle.StateFailedClosed,
				Answers: []bundle.Answer{},
			},
		},
	}
}

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

func serve(t *testing.T, b *bundle.Bundle, req *dns.Msg) *dns.Msg {
	t.Helper()
	h := New(fixedStore{b}, nil)
	w := &fakeWriter{}
	h.ServeDNS(w, req)
	if w.msg == nil {
		t.Fatal("handler did not write a response")
	}
	return w.msg
}

func TestAnswerA_OkStateMultiAnswer(t *testing.T) {
	resp := serve(t, testBundle(), query("mysql-01-rw.prod.app1.z-backv.local", dns.TypeA))

	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %v, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 3 {
		t.Fatalf("got %d answers, want 3: %v", len(resp.Answer), resp.Answer)
	}
	wantIPs := map[string]bool{"10.0.5.5": true, "10.0.5.6": true, "10.0.5.7": true}
	for _, rr := range resp.Answer {
		a, ok := rr.(*dns.A)
		if !ok {
			t.Fatalf("answer RR is not an A record: %T", rr)
		}
		if a.Hdr.Ttl != 7 {
			t.Errorf("ttl = %d, want 7", a.Hdr.Ttl)
		}
		if !wantIPs[a.A.String()] {
			t.Errorf("unexpected answer IP %s", a.A.String())
		}
	}
	if resp.RecursionAvailable {
		t.Error("RA must never be set")
	}
}

func TestAnswerA_DegradedStateStillAnswers(t *testing.T) {
	resp := serve(t, testBundle(), query("shared-cache.app1.z-backv.local", dns.TypeA))

	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %v, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(resp.Answer))
	}
	a := resp.Answer[0].(*dns.A)
	if a.A.String() != "10.0.9.9" || a.Hdr.Ttl != 5 {
		t.Errorf("got %s ttl=%d, want 10.0.9.9 ttl=5", a.A.String(), a.Hdr.Ttl)
	}
}

func TestAnswerA_FailedClosedIsServfail(t *testing.T) {
	resp := serve(t, testBundle(), query("down-service.app1.z-backv.local", dns.TypeA))

	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %v, want SERVFAIL", dns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 0 {
		t.Errorf("SERVFAIL response should carry no answers, got %d", len(resp.Answer))
	}
}

func TestAnswerA_UnknownNameUnderZoneIsNxdomainWithSOA(t *testing.T) {
	resp := serve(t, testBundle(), query("nope.app1.z-backv.local", dns.TypeA))

	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %v, want NXDOMAIN", dns.RcodeToString[resp.Rcode])
	}
	if len(resp.Ns) != 1 {
		t.Fatalf("authority section: got %d records, want 1 SOA", len(resp.Ns))
	}
	soa, ok := resp.Ns[0].(*dns.SOA)
	if !ok {
		t.Fatalf("authority record is not SOA: %T", resp.Ns[0])
	}
	if soa.Hdr.Ttl != 5 || soa.Minttl != 5 {
		t.Errorf("negative TTL = %d/%d, want 5/5", soa.Hdr.Ttl, soa.Minttl)
	}
}

func TestAnswerAAAA_KnownNameIsCleanNoerrorEmpty(t *testing.T) {
	resp := serve(t, testBundle(), query("mysql-01-rw.prod.app1.z-backv.local", dns.TypeAAAA))

	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %v, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 0 {
		t.Errorf("AAAA answer section should be empty, got %d records", len(resp.Answer))
	}
}

func TestAnswerAAAA_FailedClosedRecordStillNoerrorEmpty(t *testing.T) {
	// The musl AAAA rule applies regardless of record state (§8) — even a
	// failed-closed record's AAAA query must be clean NOERROR, not SERVFAIL.
	resp := serve(t, testBundle(), query("down-service.app1.z-backv.local", dns.TypeAAAA))

	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %v, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 0 {
		t.Errorf("AAAA answer section should be empty, got %d records", len(resp.Answer))
	}
}

func TestAnswerAAAA_UnknownNameIsNxdomain(t *testing.T) {
	resp := serve(t, testBundle(), query("nope.app1.z-backv.local", dns.TypeAAAA))

	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %v, want NXDOMAIN", dns.RcodeToString[resp.Rcode])
	}
}

func TestOutOfZoneNameIsRefused(t *testing.T) {
	resp := serve(t, testBundle(), query("example.com", dns.TypeA))

	if resp.Rcode != dns.RcodeRefused {
		t.Fatalf("rcode = %v, want REFUSED", dns.RcodeToString[resp.Rcode])
	}
}

func TestNoTargetsStateIsServfail(t *testing.T) {
	b := testBundle()
	b.Records = append(b.Records, bundle.Record{
		FQDN:  "orphan.app1.z-backv.local",
		TTL:   5,
		State: bundle.StateNoTargets,
	})
	resp := serve(t, b, query("orphan.app1.z-backv.local", dns.TypeA))

	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %v, want SERVFAIL for undocumented state %q", dns.RcodeToString[resp.Rcode], bundle.StateNoTargets)
	}
}
