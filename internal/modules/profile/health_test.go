package profile

import "testing"

func TestProfileHealthFindings(t *testing.T) {
	h := &profileHealth{}
	if findings := h.Check(t.Context()); len(findings) != 0 {
		t.Fatalf("fresh findings = %+v", findings)
	}
	for range failingThreshold {
		h.observe(&Observation{Kind: ObserveExtraction, Result: ResultFailed})
	}
	h.observe(&Observation{Kind: ObserveExtraction, Result: ResultDropped})
	h.observe(&Observation{Kind: ObserveContext})
	if findings := h.Check(t.Context()); len(findings) != 1 || findings[0].Code != "profile_extraction_failing" {
		t.Fatalf("findings = %+v, want profile_extraction_failing", findings)
	}
	h.observe(&Observation{Kind: ObserveExtraction, Result: ResultCompleted})
	if findings := h.Check(t.Context()); len(findings) != 0 {
		t.Fatalf("findings after success = %+v", findings)
	}
	h.unavailable.Store(true)
	if findings := h.Check(t.Context()); len(findings) != 1 || findings[0].Code != "profile_extractor_unavailable" {
		t.Fatalf("findings = %+v, want profile_extractor_unavailable", findings)
	}
}
