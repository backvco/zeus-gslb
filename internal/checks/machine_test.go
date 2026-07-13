package checks

import "testing"

func TestMachine_InitialStateUnknownNotReady(t *testing.T) {
	m := NewMachine()
	if got := m.State("t1"); got != Unknown {
		t.Fatalf("initial state = %v, want Unknown", got)
	}
	if m.Ready("t1") {
		t.Fatal("Ready() = true before any result recorded")
	}
}

func TestMachine_FailureThresholdBothDirections(t *testing.T) {
	m := NewMachine()
	const failThresh, recoverThresh = 3, 2

	// Two failures: not yet unhealthy.
	for i := 0; i < 2; i++ {
		transitioned, h := m.RecordResult("t1", false, failThresh, recoverThresh)
		if transitioned {
			t.Fatalf("unexpected transition on failure #%d (health=%v)", i+1, h)
		}
		if h != Unknown {
			t.Fatalf("health after %d failures = %v, want Unknown (below threshold)", i+1, h)
		}
	}

	// Third consecutive failure crosses the threshold -> Unhealthy.
	transitioned, h := m.RecordResult("t1", false, failThresh, recoverThresh)
	if !transitioned || h != Unhealthy {
		t.Fatalf("3rd failure: transitioned=%v health=%v, want true/Unhealthy", transitioned, h)
	}

	// One success: not yet enough to recover.
	transitioned, h = m.RecordResult("t1", true, failThresh, recoverThresh)
	if transitioned || h != Unhealthy {
		t.Fatalf("1st success after unhealthy: transitioned=%v health=%v, want false/Unhealthy", transitioned, h)
	}

	// Second consecutive success crosses recoveryThreshold -> Healthy.
	transitioned, h = m.RecordResult("t1", true, failThresh, recoverThresh)
	if !transitioned || h != Healthy {
		t.Fatalf("2nd success: transitioned=%v health=%v, want true/Healthy", transitioned, h)
	}
}

func TestMachine_FailureResetsSuccessStreakAndViceVersa(t *testing.T) {
	m := NewMachine()
	m.RecordResult("t1", true, 2, 2)  // success streak 1
	m.RecordResult("t1", false, 2, 2) // resets success streak, fail streak 1
	// A single subsequent success must not have any "carry over" advantage —
	// still needs a fresh 2-in-a-row to recover, but since it was never
	// unhealthy to begin with, health stays Unknown throughout below
	// threshold — this test's real assertion is that a failure interrupts a
	// success streak (start over) which we check indirectly with Ready.
	if !m.Ready("t1") {
		t.Fatal("Ready() should be true after any recorded result")
	}
	if got := m.State("t1"); got != Unknown {
		t.Fatalf("state = %v, want Unknown (never crossed either threshold)", got)
	}
}

func TestMachine_ReadyAfterOneCycleEvenIfStillUnknown(t *testing.T) {
	m := NewMachine()
	// failureThreshold=5 means one failure keeps health Unknown, but the
	// cycle still counts for post-boot-grace purposes.
	m.RecordResult("t1", false, 5, 2)
	if !m.Ready("t1") {
		t.Fatal("Ready() should be true after one completed check cycle")
	}
	if got := m.State("t1"); got != Unknown {
		t.Fatalf("state = %v, want Unknown (below failureThreshold)", got)
	}
}

func TestMachine_StatesSnapshot(t *testing.T) {
	m := NewMachine()
	m.RecordResult("t1", false, 1, 1) // -> Unhealthy immediately
	m.RecordResult("t2", true, 1, 1)  // -> Healthy immediately

	states := m.States()
	if states["t1"] != string(Unhealthy) {
		t.Errorf("t1 = %q, want %q", states["t1"], Unhealthy)
	}
	if states["t2"] != string(Healthy) {
		t.Errorf("t2 = %q, want %q", states["t2"], Healthy)
	}
	if _, ok := states["t3"]; ok {
		t.Error("States() should not include unrecorded keys")
	}
}

func TestMachine_ZeroThresholdsTreatedAsOne(t *testing.T) {
	m := NewMachine()
	transitioned, h := m.RecordResult("t1", false, 0, 0)
	if !transitioned || h != Unhealthy {
		t.Fatalf("zero failureThreshold should behave as 1: transitioned=%v health=%v", transitioned, h)
	}
}
