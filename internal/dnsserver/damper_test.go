package dnsserver

import (
	"testing"
	"time"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestDamper_FirstAnswerAlwaysApplied(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	d := NewDamper(c.now)
	a := []bundle.Answer{ans("10.0.0.1", 1)}
	got := d.Evaluate("f", a, false)
	if !answerSetEqual(got, a) {
		t.Fatalf("first evaluate should apply candidate verbatim, got %v", got)
	}
}

func TestDamper_FreezesOnFourthChangeWithin10Min(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	d := NewDamper(c.now)

	sets := [][]bundle.Answer{
		{ans("10.0.0.1", 1)},
		{ans("10.0.0.2", 1)}, // change 1
		{ans("10.0.0.3", 1)}, // change 2
		{ans("10.0.0.4", 1)}, // change 3
		{ans("10.0.0.5", 1)}, // change 4 -> should be frozen
	}

	var last []bundle.Answer
	for i, s := range sets {
		got := d.Evaluate("f", s, false)
		if i < 4 {
			if !answerSetEqual(got, s) {
				t.Fatalf("change %d should apply, got %v want %v", i, got, s)
			}
			last = got
		} else {
			if answerSetEqual(got, s) {
				t.Fatalf("4th change should be frozen/suppressed, but got the new set %v", got)
			}
			if !answerSetEqual(got, last) {
				t.Fatalf("frozen answer should equal last-applied set %v, got %v", last, got)
			}
		}
		c.advance(time.Second)
	}
	if !d.Frozen("f") {
		t.Fatal("expected record to be frozen after 4th change")
	}
}

func TestDamper_ChangesOutsideWindowDoNotCountTowardFreeze(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	d := NewDamper(c.now)

	d.Evaluate("f", []bundle.Answer{ans("10.0.0.1", 1)}, false)
	d.Evaluate("f", []bundle.Answer{ans("10.0.0.2", 1)}, false)        // change 1
	c.advance(11 * time.Minute)                                        // change 1 now outside the 10-min window
	d.Evaluate("f", []bundle.Answer{ans("10.0.0.3", 1)}, false)        // change 2 (only 1 in-window)
	d.Evaluate("f", []bundle.Answer{ans("10.0.0.4", 1)}, false)        // change 3 (2 in-window)
	got := d.Evaluate("f", []bundle.Answer{ans("10.0.0.5", 1)}, false) // change 4 (3 in-window) — should still apply
	if !answerSetEqual(got, []bundle.Answer{ans("10.0.0.5", 1)}) {
		t.Fatalf("with only 3 changes inside the rolling window, should not freeze; got %v", got)
	}
	if d.Frozen("f") {
		t.Fatal("should not be frozen — only 3 changes fell inside the 10-min window")
	}
}

func TestDamper_ForcedMoveBypassesFreezeAndIsNotCountedAsAChange(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	d := NewDamper(c.now)

	d.Evaluate("f", []bundle.Answer{ans("10.0.0.1", 1)}, false)
	d.Evaluate("f", []bundle.Answer{ans("10.0.0.2", 1)}, false) // change 1
	d.Evaluate("f", []bundle.Answer{ans("10.0.0.3", 1)}, false) // change 2
	d.Evaluate("f", []bundle.Answer{ans("10.0.0.4", 1)}, false) // change 3

	// A forced move now — must apply even though the window already has 3
	// changes (a non-forced 4th would have frozen).
	got := d.Evaluate("f", []bundle.Answer{ans("10.0.0.5", 1)}, true)
	if !answerSetEqual(got, []bundle.Answer{ans("10.0.0.5", 1)}) {
		t.Fatalf("forced move must apply immediately, got %v", got)
	}
	if d.Frozen("f") {
		t.Fatal("a forced move must not itself trigger a freeze")
	}

	// Now a 4th NON-forced change (this is now the 4th accepted-window
	// change if the forced one didn't count, or would freeze if it did) —
	// per contract, forced moves are not counted toward the window, so this
	// is only the 4th regular change and should freeze.
	got2 := d.Evaluate("f", []bundle.Answer{ans("10.0.0.6", 1)}, false)
	if answerSetEqual(got2, []bundle.Answer{ans("10.0.0.6", 1)}) {
		t.Fatalf("expected this non-forced change to be frozen (forced move must not count toward the window), got %v", got2)
	}
}

func TestDamper_AutoUnfreezeAfter20MinQuiet(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	d := NewDamper(c.now)

	d.Evaluate("f", []bundle.Answer{ans("10.0.0.1", 1)}, false)
	d.Evaluate("f", []bundle.Answer{ans("10.0.0.2", 1)}, false)
	d.Evaluate("f", []bundle.Answer{ans("10.0.0.3", 1)}, false)
	d.Evaluate("f", []bundle.Answer{ans("10.0.0.4", 1)}, false)
	frozenAt := d.Evaluate("f", []bundle.Answer{ans("10.0.0.5", 1)}, false) // frozen
	if !d.Frozen("f") {
		t.Fatal("expected frozen after 4th change")
	}

	c.advance(19 * time.Minute)
	stillFrozen := d.Evaluate("f", []bundle.Answer{ans("10.0.0.6", 1)}, false) // suppressed attempt resets quiet clock
	if !d.Frozen("f") {
		t.Fatal("should still be frozen before 20min quiet elapses")
	}
	if !answerSetEqual(stillFrozen, frozenAt) {
		t.Fatalf("should still serve frozen set, got %v want %v", stillFrozen, frozenAt)
	}

	// A further suppressed attempt at t=19min RESETS the quiet clock (per
	// "20 minutes WITHOUT suppressed changes") — so 20 more minutes must
	// elapse from there, not from the original freeze.
	c.advance(20 * time.Minute)
	unfrozen := d.Evaluate("f", []bundle.Answer{ans("10.0.0.7", 1)}, false)
	if !answerSetEqual(unfrozen, []bundle.Answer{ans("10.0.0.7", 1)}) {
		t.Fatalf("expected auto-unfreeze to apply the new change, got %v", unfrozen)
	}
	if d.Frozen("f") {
		t.Fatal("expected auto-unfreeze to have cleared the frozen flag")
	}
}
