package experiment

import "testing"

// The measured core, at test scale: replay must be deterministic for every
// trace, every faulted wrong answer must be attributed to the span that
// was actually faulted, and no unfaulted request may answer wrong.
func TestWorkloadInvariants(t *testing.T) {
	res, err := Run(t.TempDir(), 48, 20261023)
	if err != nil {
		t.Fatal(err)
	}
	if res.WrongAnswers == 0 {
		t.Fatal("workload produced no wrong answers; fault injection broken")
	}
	if res.WrongAmongUnfaulted != 0 {
		t.Fatalf("%d unfaulted requests answered wrong", res.WrongAmongUnfaulted)
	}
	if res.ReplayDeterminism != 1.0 {
		t.Fatalf("replay determinism = %v, want 1.0 (%d/%d identical)",
			res.ReplayDeterminism, res.ReplayIdentical, res.Requests)
	}
	if res.AttributionAccuracy != 1.0 {
		t.Fatalf("attribution accuracy = %v, want 1.0 (%d correct)",
			res.AttributionAccuracy, res.AttributionCorrect)
	}
	if res.SurgeryFixRate != 1.0 {
		t.Fatalf("surgery fix rate = %v, want 1.0", res.SurgeryFixRate)
	}
	if res.BaselineAccuracy > res.AttributionAccuracy {
		t.Fatalf("log-only baseline (%v) beat traceweave (%v)", res.BaselineAccuracy, res.AttributionAccuracy)
	}
	t.Logf("baseline accuracy at n=48: %v (%d/%d)", res.BaselineAccuracy, res.BaselineCorrect, res.WrongAnswers)
}
