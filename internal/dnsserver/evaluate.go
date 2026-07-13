package dnsserver

import (
	"sync"
	"time"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

// LocalHealth is the subset of *checks.Machine the candidate-walk
// evaluator needs — narrowed to an interface so evaluator tests don't
// depend on the checks package's probe/runner machinery.
type LocalHealth interface {
	// State returns targetKey's current locally-observed health
	// (checks.Healthy/Unhealthy/Unknown as a string) — Unknown for any key
	// never recorded.
	State(targetKey string) string
	// Ready reports whether targetKey has completed at least one full
	// local check cycle (post-boot grace, P2 contract).
	Ready(targetKey string) bool
}

// healthUnhealthy/healthUnknown mirror checks.Unhealthy/checks.Unknown as
// plain strings, matching both LocalHealth.State's return values and
// bundle.Candidate.Health's wire values — kept local to avoid an import
// cycle-shaped dependency from dnsserver back to checks beyond this
// narrow interface.
const (
	healthUnhealthy = "unhealthy"
	healthUnknown   = "unknown"
)

// Evaluator implements the P2 contract's "Responder answer evaluation"
// candidate walk plus its local damping mirror (plan §7). One Evaluator is
// shared by the Handler across all queries for a store; it holds per-fqdn
// state (previously-active candidates, for forced-move detection, and the
// Damper's per-fqdn history) across calls.
type Evaluator struct {
	machine LocalHealth
	damper  *Damper

	mu         sync.Mutex
	activeKeys map[string][]string // fqdn -> targetKeys currently contributing to the served answer set
}

// NewEvaluator builds an Evaluator. machine may be nil — every candidate is
// then treated as a remote candidate (no local check gating), which is
// harmless for bundles that never carry a Candidates field at all (the
// legacy path never consults machine).
func NewEvaluator(machine LocalHealth, now func() time.Time) *Evaluator {
	return &Evaluator{
		machine:    machine,
		damper:     NewDamper(now),
		activeKeys: make(map[string][]string),
	}
}

// Resolve returns the answers to serve for rec, and ok=false when the
// record must SERVFAIL (P2 contract: "Zero viable → SERVFAIL"). When
// rec.Candidates is nil (the JSON key was entirely absent — a v1.0 bundle),
// this is the legacy path: serve rec.Answers per rec.State exactly as
// before v1.1.
func (e *Evaluator) Resolve(rec bundle.Record) (answers []bundle.Answer, ok bool) {
	if rec.Candidates == nil {
		switch rec.State {
		case bundle.StateOK, bundle.StateDegraded:
			return rec.Answers, true
		default:
			return nil, false
		}
	}

	localTargets := make(map[string]bool, len(rec.LocalChecks))
	for _, lc := range rec.LocalChecks {
		localTargets[lc.TargetKey] = true
	}

	e.mu.Lock()
	prevKeys := e.activeKeys[rec.FQDN]
	e.mu.Unlock()
	prevActive := make(map[string]bool, len(prevKeys))
	for _, k := range prevKeys {
		prevActive[k] = true
	}

	all := rec.Mode == bundle.ModeAll

	var viableKeys []string
	var viableAnswerGroups [][]bundle.Answer
	forced := false

	for _, cand := range rec.Candidates {
		viable, effectiveUnhealthy := e.viable(cand, localTargets)
		if prevActive[cand.TargetKey] && effectiveUnhealthy {
			// A candidate that was contributing to the served set just
			// became (effectively) unhealthy — any resulting answer-set
			// change is a forced move (plan §7), never damped.
			forced = true
		}
		// A candidate with no addresses cannot serve traffic, whatever its
		// health says. Counting it "viable" made us return NOERROR with an
		// empty answer set when every backend was down — a resolver reads
		// that as "this name exists and has no address" and caches it, so
		// the record never fails closed. zeus already filters answerless
		// candidates out of the bundle; this is the independent second
		// guard, so a compiler regression can only ever cost us SERVFAIL
		// (retryable) and never a wrong/empty NOERROR.
		if viable && len(cand.Answers) == 0 {
			continue
		}
		if viable {
			viableKeys = append(viableKeys, cand.TargetKey)
			viableAnswerGroups = append(viableAnswerGroups, cand.Answers)
			if !all {
				break // mode:"single" — first viable candidate wins
			}
		}
	}

	if len(viableAnswerGroups) == 0 {
		return nil, false
	}

	var union []bundle.Answer
	for _, g := range viableAnswerGroups {
		union = append(union, g...)
	}

	served := e.damper.Evaluate(rec.FQDN, union, forced)

	e.mu.Lock()
	if answerSetEqual(served, union) {
		// The damper actually applied this round's candidate set (not
		// suppressed by a freeze) — record it as the new active set.
		e.activeKeys[rec.FQDN] = viableKeys
	}
	e.mu.Unlock()

	return served, true
}

// viable evaluates one candidate per the P2 contract's rule:
//
//	has a localCheck  -> local state machine says NOT unhealthy (a local
//	                     Unknown that hasn't completed its first check
//	                     cycle yet defers to the pushed health value —
//	                     post-boot grace)
//	no localCheck     -> pushed health != unhealthy
//
// It also returns whether the EFFECTIVE health used was unhealthy, for the
// caller's forced-move bookkeeping.
func (e *Evaluator) viable(cand bundle.Candidate, localTargets map[string]bool) (viable bool, effectiveUnhealthy bool) {
	if localTargets[cand.TargetKey] && e.machine != nil {
		local := e.machine.State(cand.TargetKey)
		effective := local
		if local == healthUnknown && !e.machine.Ready(cand.TargetKey) {
			effective = cand.Health // post-boot grace: defer to pushed value
		}
		return effective != healthUnhealthy, effective == healthUnhealthy
	}
	return cand.Health != healthUnhealthy, cand.Health == healthUnhealthy
}
