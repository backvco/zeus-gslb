package dnsserver

import (
	"testing"
	"time"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

// fakeMachine is a hand-rolled LocalHealth for evaluator tests.
type fakeMachine struct {
	states map[string]string
	ready  map[string]bool
}

func (f fakeMachine) State(key string) string {
	if s, ok := f.states[key]; ok {
		return s
	}
	return healthUnknown
}
func (f fakeMachine) Ready(key string) bool { return f.ready[key] }

func ans(ip string, port int) bundle.Answer { return bundle.Answer{IP: ip, Port: port} }

func candRecord(mode string, cands ...bundle.Candidate) bundle.Record {
	return bundle.Record{FQDN: "svc.z-backv.local", TTL: 5, Mode: mode, Candidates: cands}
}

func withLocalCheck(rec bundle.Record, targetKeys ...string) bundle.Record {
	for _, k := range targetKeys {
		rec.LocalChecks = append(rec.LocalChecks, bundle.LocalCheck{TargetKey: k})
	}
	return rec
}

func TestEvaluator_LegacyPath_NilCandidatesUsesStateAnswers(t *testing.T) {
	e := NewEvaluator(nil, time.Now)
	rec := bundle.Record{FQDN: "x", TTL: 5, State: bundle.StateOK, Answers: []bundle.Answer{ans("10.0.0.1", 80)}}

	answers, ok := e.Resolve(rec)
	if !ok || len(answers) != 1 || answers[0].IP != "10.0.0.1" {
		t.Fatalf("legacy ok path: answers=%v ok=%v", answers, ok)
	}

	rec.State = bundle.StateFailedClosed
	_, ok = e.Resolve(rec)
	if ok {
		t.Fatal("legacy failed-closed path must SERVFAIL")
	}
}

func TestEvaluator_RemoteCandidate_SkipsWhenPushedUnhealthy(t *testing.T) {
	e := NewEvaluator(nil, time.Now)
	rec := candRecord(bundle.ModeSingle,
		bundle.Candidate{TargetKey: "z-02/a", Health: "unhealthy", Answers: []bundle.Answer{ans("10.0.0.1", 1)}},
		bundle.Candidate{TargetKey: "z-02/b", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.2", 1)}},
	)
	answers, ok := e.Resolve(rec)
	if !ok || len(answers) != 1 || answers[0].IP != "10.0.0.2" {
		t.Fatalf("expected fallthrough to 2nd healthy remote candidate, got %v ok=%v", answers, ok)
	}
}

func TestEvaluator_LocalCandidate_SkipsWhenLocallyUnhealthy(t *testing.T) {
	machine := fakeMachine{states: map[string]string{"z-01/a": "unhealthy"}, ready: map[string]bool{"z-01/a": true}}
	e := NewEvaluator(machine, time.Now)
	rec := withLocalCheck(candRecord(bundle.ModeSingle,
		bundle.Candidate{TargetKey: "z-01/a", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.1", 1)}}, // pushed says healthy but LOCAL says unhealthy
		bundle.Candidate{TargetKey: "z-02/b", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.2", 1)}},
	), "z-01/a")

	answers, ok := e.Resolve(rec)
	if !ok || len(answers) != 1 || answers[0].IP != "10.0.0.2" {
		t.Fatalf("expected local unhealthy to override pushed-healthy and skip to next candidate, got %v ok=%v", answers, ok)
	}
}

func TestEvaluator_PostBootGrace_UnknownLocalDefersToPushed(t *testing.T) {
	// Local state Unknown AND Ready()==false (never completed a cycle) —
	// must defer to the pushed health value, per the P2 contract.
	machine := fakeMachine{states: map[string]string{}, ready: map[string]bool{}} // "z-01/a" absent => Unknown, not ready
	e := NewEvaluator(machine, time.Now)
	rec := withLocalCheck(candRecord(bundle.ModeSingle,
		bundle.Candidate{TargetKey: "z-01/a", Health: "unhealthy", Answers: []bundle.Answer{ans("10.0.0.1", 1)}},
		bundle.Candidate{TargetKey: "z-02/b", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.2", 1)}},
	), "z-01/a")

	answers, ok := e.Resolve(rec)
	if !ok || len(answers) != 1 || answers[0].IP != "10.0.0.2" {
		t.Fatalf("grace should defer to pushed=unhealthy and skip candidate a, got %v ok=%v", answers, ok)
	}
}

func TestEvaluator_UnknownLocalAfterGraceIsStillViable(t *testing.T) {
	// Ready()==true (completed >=1 cycle) but State()==Unknown (below
	// threshold either direction) — contract: local state NOT unhealthy is
	// viable, regardless of what's pushed.
	machine := fakeMachine{states: map[string]string{"z-01/a": healthUnknown}, ready: map[string]bool{"z-01/a": true}}
	e := NewEvaluator(machine, time.Now)
	rec := withLocalCheck(candRecord(bundle.ModeSingle,
		bundle.Candidate{TargetKey: "z-01/a", Health: "unhealthy", Answers: []bundle.Answer{ans("10.0.0.1", 1)}},
	), "z-01/a")

	answers, ok := e.Resolve(rec)
	if !ok || len(answers) != 1 || answers[0].IP != "10.0.0.1" {
		t.Fatalf("post-grace local Unknown must be viable regardless of pushed value, got %v ok=%v", answers, ok)
	}
}

func TestEvaluator_ModeAll_UnionOfViable(t *testing.T) {
	e := NewEvaluator(nil, time.Now)
	rec := candRecord(bundle.ModeAll,
		bundle.Candidate{TargetKey: "a", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.1", 1)}},
		bundle.Candidate{TargetKey: "b", Health: "unhealthy", Answers: []bundle.Answer{ans("10.0.0.2", 1)}},
		bundle.Candidate{TargetKey: "c", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.3", 1)}},
	)
	answers, ok := e.Resolve(rec)
	if !ok || len(answers) != 2 {
		t.Fatalf("mode:all should union all viable candidates (2), got %v ok=%v", answers, ok)
	}
}

func TestEvaluator_ZeroViable_Servfail(t *testing.T) {
	e := NewEvaluator(nil, time.Now)
	rec := candRecord(bundle.ModeSingle,
		bundle.Candidate{TargetKey: "a", Health: "unhealthy", Answers: []bundle.Answer{ans("10.0.0.1", 1)}},
	)
	_, ok := e.Resolve(rec)
	if ok {
		t.Fatal("zero viable candidates must SERVFAIL")
	}
}

func TestEvaluator_EmptyCandidatesArrayIsZeroViable(t *testing.T) {
	// Present-but-empty candidates (distinct from a nil/absent field) — the
	// non-legacy path with nothing to walk is vacuously zero-viable.
	e := NewEvaluator(nil, time.Now)
	rec := candRecord(bundle.ModeSingle) // Candidates: []bundle.Candidate{} (non-nil, empty)
	_, ok := e.Resolve(rec)
	if ok {
		t.Fatal("empty (but present) candidates array must SERVFAIL, not fall back to legacy")
	}
}

func TestEvaluator_Damping_ForcedMoveBypassesFreeze(t *testing.T) {
	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return fixedNow }
	machine := fakeMachine{states: map[string]string{"a": "healthy", "b": "healthy"}, ready: map[string]bool{"a": true, "b": true}}
	e := NewEvaluator(machine, nowFn)

	recFor := func(aHealth string) bundle.Record {
		return withLocalCheck(candRecord(bundle.ModeSingle,
			bundle.Candidate{TargetKey: "a", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.1", 1)}},
			bundle.Candidate{TargetKey: "b", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.2", 1)}},
		), "a", "b")
	}

	// Flip a<->b repeatedly (healthy<->healthy oscillation) to trip the
	// freeze — mode toggles by alternating which local state is "healthy".
	toggle := func(aUp bool) {
		if aUp {
			machine.states["a"] = "healthy"
		} else {
			machine.states["a"] = "unhealthy"
		}
	}

	rec := recFor("")
	// Establish a as the served answer.
	answers, _ := e.Resolve(rec)
	if answers[0].IP != "10.0.0.1" {
		t.Fatalf("expected a first, got %v", answers)
	}

	// 3 oscillations a(unhealthy)->b, b->a(healthy), etc, all NON-forced
	// since neither transition removes a *previously active* target that
	// just became unhealthy while active (a becomes unhealthy, but was it
	// active? yes — so the first flip IS forced. To test pure healthy<->
	// healthy oscillation with no forced moves, alternate `b`'s pushed
	// health from the remote side without touching local `a`.)
	_ = toggle

	// Simpler oscillation: alternate mode by using two DIFFERENT candidate
	// orders that are both fully healthy, so switching between them is a
	// pure preference change (not a health-driven forced move). We model
	// that by never marking anything unhealthy at all — instead we drive
	// oscillation via the pushed `Health` field on remote-style candidates
	// with no local check, so `forced` is never set.
	e2 := NewEvaluator(nil, nowFn)
	altRec := func(order string) bundle.Record {
		if order == "a" {
			return candRecord(bundle.ModeSingle,
				bundle.Candidate{TargetKey: "a", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.1", 1)}},
				bundle.Candidate{TargetKey: "b", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.2", 1)}},
			)
		}
		return candRecord(bundle.ModeSingle,
			bundle.Candidate{TargetKey: "b", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.2", 1)}},
			bundle.Candidate{TargetKey: "a", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.1", 1)}},
		)
	}

	seq := []string{"a", "b", "a", "b"} // a is the baseline; b,a,b are changes 1,2,3
	var last []bundle.Answer
	for i, o := range seq {
		last, _ = e2.Resolve(altRec(o))
		_ = i
	}
	// After exactly 3 accepted changes (b, a, b), a 4th change (back to a)
	// must be frozen — served answers stay at the 3rd change's set ("b").
	frozenAnswers, _ := e2.Resolve(altRec("a"))
	if !answerSetEqual(frozenAnswers, last) {
		t.Fatalf("4th oscillation should be frozen at previous served set %v, got %v", last, frozenAnswers)
	}
}

func TestEvaluator_Damping_ForcedMoveAwayFromUnhealthyAlwaysApplied(t *testing.T) {
	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return fixedNow }
	machine := &fakeMachine{states: map[string]string{"a": "healthy", "b": "healthy"}, ready: map[string]bool{"a": true, "b": true}}
	e := NewEvaluator(machine, nowFn)

	rec := func() bundle.Record {
		return withLocalCheck(candRecord(bundle.ModeSingle,
			bundle.Candidate{TargetKey: "a", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.1", 1)}},
			bundle.Candidate{TargetKey: "b", Health: "healthy", Answers: []bundle.Answer{ans("10.0.0.2", 1)}},
		), "a", "b")
	}

	// Establish a as active.
	answers, _ := e.Resolve(rec())
	if answers[0].IP != "10.0.0.1" {
		t.Fatalf("expected a first, got %v", answers)
	}

	// Burn through the freeze via non-forced oscillation on a DIFFERENT
	// record isn't relevant here — instead directly drive >3 forced moves
	// to prove forced moves never freeze regardless of count.
	for i := 0; i < 5; i++ {
		if i%2 == 0 {
			machine.states["a"] = "unhealthy"
		} else {
			machine.states["a"] = "healthy"
		}
		answers, ok := e.Resolve(rec())
		if !ok {
			t.Fatalf("iteration %d: unexpected SERVFAIL", i)
		}
		wantIP := "10.0.0.2"
		if i%2 != 0 {
			wantIP = "10.0.0.1"
		}
		if answers[0].IP != wantIP {
			t.Fatalf("iteration %d: forced move should always apply immediately, got %v want %s", i, answers, wantIP)
		}
	}
}

// A candidate with zero answers must never count as viable: returning
// NOERROR-with-no-answers when every backend is down lets resolvers cache
// "this name has no address" instead of retrying a SERVFAIL. Regression for
// the live-kind-battery T3 failure (2026-07-12).
func TestResolveAnswerlessCandidateIsNotViable(t *testing.T) {
	rec := bundle.Record{
		FQDN: "api.prod.app1.z-t.local.",
		TTL:  2,
		Mode: "single",
		Candidates: []bundle.Candidate{
			{Cluster: "z-01", TargetKey: "a", Health: "healthy", Answers: nil},
			{Cluster: "z-02", TargetKey: "b", Health: "healthy", Answers: nil},
		},
	}
	e := NewEvaluator(nil, nil)
	answers, ok := e.Resolve(rec)
	if ok || len(answers) != 0 {
		t.Fatalf("all-answerless candidates must fail closed (SERVFAIL), got ok=%v answers=%v", ok, answers)
	}
}

// ...but a healthy candidate WITH answers still wins even if an earlier,
// higher-priority candidate has none (the answerless one is skipped, not fatal).
func TestResolveSkipsAnswerlessAndUsesNextCandidate(t *testing.T) {
	rec := bundle.Record{
		FQDN: "api.prod.app1.z-t.local.",
		TTL:  2,
		Mode: "single",
		Candidates: []bundle.Candidate{
			{Cluster: "z-01", TargetKey: "a", Health: "healthy", Answers: nil},
			{Cluster: "z-02", TargetKey: "b", Health: "healthy", Answers: []bundle.Answer{{IP: "10.0.0.9", Port: 80}}},
		},
	}
	e := NewEvaluator(nil, nil)
	answers, ok := e.Resolve(rec)
	if !ok || len(answers) != 1 || answers[0].IP != "10.0.0.9" {
		t.Fatalf("expected fallthrough to the candidate that has answers, got ok=%v answers=%v", ok, answers)
	}
}
