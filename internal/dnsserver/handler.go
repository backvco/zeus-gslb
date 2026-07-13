// Package dnsserver implements the responder-mode DNS handler: it answers
// queries for the global zone straight out of the in-memory bundle (plan
// §8), and nothing else. No recursion, no upstream forwarding — the
// zeus-overlay-dns Corefile is what forwards the global zone's UDP/TCP
// traffic to this responder (`forward . 127.0.0.1:5355`); anything this
// handler can't answer for gets REFUSED or SERVFAIL, never proxied further.
package dnsserver

import (
	"log"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

// negativeTTL is the TTL on the SOA record returned in the authority section
// for NXDOMAIN answers (plan §8: "explicit low negative TTL").
const negativeTTL = 5

// Store is the subset of *bundle.Store the handler needs — narrowed to an
// interface so handler tests can supply a fixed bundle without the file/
// polling machinery.
type Store interface {
	Current() *bundle.Bundle
}

// Handler answers DNS queries for one global zone from a bundle.Store.
type Handler struct {
	store     Store
	evaluator *Evaluator
}

// New builds a Handler serving the given store. machine is this cluster's
// local health state machine (nil is fine — every bundle used in v0/v1.0-
// only tests has no Candidates field, so the legacy path never consults
// it); it gates candidate viability in bundle v1.1's candidate-walk path
// (P2 contract).
func New(store Store, machine LocalHealth) *Handler {
	return &Handler{store: store, evaluator: NewEvaluator(machine, time.Now)}
}

// ServeDNS implements dns.Handler.
func (h *Handler) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(req)
	msg.Authoritative = true
	msg.RecursionAvailable = false // never offer recursion (§8)

	if len(req.Question) != 1 {
		// A responder for a single purpose-built zone has no reason to
		// support multi-question messages; refuse rather than guess.
		msg.Rcode = dns.RcodeRefused
		_ = w.WriteMsg(msg)
		return
	}
	q := req.Question[0]

	b := h.store.Current()
	if b == nil {
		// Should be unreachable — Store always boots with a bundle or fails
		// to start — but never SERVFAIL-crash on a nil map lookup.
		msg.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(msg)
		return
	}

	zone := dns.Fqdn(b.Domain)
	name := dns.Fqdn(q.Name)

	if !dns.IsSubDomain(zone, name) {
		msg.Rcode = dns.RcodeRefused
		_ = w.WriteMsg(msg)
		return
	}

	switch q.Qtype {
	case dns.TypeA:
		h.answerA(msg, b, zone, name)
	case dns.TypeAAAA:
		h.answerAAAA(msg, b, zone, name)
	default:
		// Anything else under the zone (MX, TXT, SRV, ...): the zone only
		// ever publishes A answers (§8: "A records, no CNAMEs") — treat as a
		// clean empty NOERROR if the name exists, NXDOMAIN if it doesn't, to
		// stay consistent with how resolvers expect "no data of this type"
		// vs. "name doesn't exist" to look.
		h.answerOther(msg, b, zone, name)
	}

	if err := w.WriteMsg(msg); err != nil {
		log.Printf("dnsserver: write response for %s %s: %v", name, dns.TypeToString[q.Qtype], err)
	}
}

func (h *Handler) answerA(msg *dns.Msg, b *bundle.Bundle, zone, name string) {
	rec, ok := lookup(b, name)
	if !ok {
		setNXDOMAIN(msg, zone, name)
		return
	}

	answers, ok := h.evaluator.Resolve(rec)
	if !ok {
		// P2 contract: zero viable candidates (v1.1 path), or a legacy-path
		// state other than ok/degraded (bundle.StateFailedClosed,
		// bundle.StateNoTargets, or any future/unrecognized value) — see
		// package bundle's doc comment on why unrecognized states fail
		// closed rather than answering blind.
		msg.Rcode = dns.RcodeServerFailure
		return
	}

	for _, ans := range answers {
		ip := net.ParseIP(ans.IP).To4()
		if ip == nil {
			log.Printf("dnsserver: record %s: skipping unparseable/non-IPv4 answer IP %q", name, ans.IP)
			continue
		}
		rr := &dns.A{
			Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: uint32(rec.TTL)},
			A:   ip,
		}
		msg.Answer = append(msg.Answer, rr)
	}
	if len(msg.Answer) == 0 {
		// An answerable record with zero (or all unparseable) answers has
		// nothing safe to serve — SERVFAIL rather than a lying empty
		// NOERROR.
		msg.Rcode = dns.RcodeServerFailure
	}
}

// answerAAAA implements the "musl requirement" (§8): AAAA for ANY known name
// under the zone gets a clean NOERROR with no records — never NXDOMAIN,
// never SERVFAIL — regardless of the record's state. Only a genuinely
// unknown name is NXDOMAIN.
func (h *Handler) answerAAAA(msg *dns.Msg, b *bundle.Bundle, zone, name string) {
	if _, ok := lookup(b, name); !ok {
		setNXDOMAIN(msg, zone, name)
		return
	}
	// Known name, no AAAA data: NOERROR with an empty answer section is the
	// zero value of msg.Answer — nothing further to do.
}

func (h *Handler) answerOther(msg *dns.Msg, b *bundle.Bundle, zone, name string) {
	if _, ok := lookup(b, name); !ok {
		setNXDOMAIN(msg, zone, name)
		return
	}
	// Known name, no data for this qtype: empty NOERROR.
}

func lookup(b *bundle.Bundle, name string) (bundle.Record, bool) {
	name = strings.ToLower(name)
	for _, r := range b.Records {
		if strings.ToLower(dns.Fqdn(r.FQDN)) == name {
			return r, true
		}
	}
	return bundle.Record{}, false
}

// setNXDOMAIN sets rcode NXDOMAIN and adds a synthesized SOA to the
// authority section with the negative-caching TTL (§8: "NXDOMAIN with
// explicit low negative TTL"). v0 has no real SOA data to publish (there is
// no separate zone-authority document yet), so this SOA is a minimal,
// internally-consistent placeholder — enough for resolvers to respect the
// negative TTL, not a claim of zone-transfer-grade SOA semantics.
func setNXDOMAIN(msg *dns.Msg, zone, queriedName string) {
	msg.Rcode = dns.RcodeNameError
	soa := &dns.SOA{
		Hdr:     dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: negativeTTL},
		Ns:      "ns." + zone,
		Mbox:    "hostmaster." + zone,
		Serial:  1,
		Refresh: negativeTTL,
		Retry:   negativeTTL,
		Expire:  negativeTTL,
		Minttl:  negativeTTL,
	}
	msg.Ns = append(msg.Ns, soa)
}
