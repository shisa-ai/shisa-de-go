package shisade

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestInjectedReadoutEchoPreservesConfiguration(t *testing.T) {
	for _, tc := range []struct {
		model, request string
		applied        bool
	}{
		{"neutral", "shisa-ai/shisa-de-2", false},
		{"shisa-ai/shisa-de-2", "de2-test", true},
	} {
		t.Run(tc.model, func(t *testing.T) {
			r, _ := newPolicyReadout(t, 2, servedBody(tc.request))
			r.model = tc.request
			m, err := NewDecisionModel(DecisionModelConfig{Model: tc.model, Tokenizer: "neutral", Family: FamilyDE2, Readout: r})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			d, err := m.Decide(context.Background(), "evidence", map[string]Question{"q": NewNoul("yes?")}, CallOptions{})
			if err != nil {
				t.Fatal(err)
			}
			a := d.Answers["q"]
			if a.Calibrated != tc.applied || a.reportProvenance || a.CalibrationWarning != "" {
				t.Fatalf("echo %+v", a)
			}
			if _, ok := d.Meta["configured_calibration"]; ok {
				t.Fatal("echo created alias provenance")
			}
			if source, _ := liveAnswerIdentity(m, a); source != ReleasedCheckpoint(tc.model) {
				t.Fatalf("live expectation identity %q", source)
			}
		})
	}
}

func TestProbeExplicitRecordReportsApplicabilitySeparately(t *testing.T) {
	record, _ := CalibrationFor(FamilyDE2)
	body := chatBody(`{"token":"A","logprob":-0.1},{"token":"B","logprob":-1.0}`)
	r, _ := newPolicyReadout(t, MaxCodes, modelsBody("shisa-ai/shisa-de-2"), servedBody("shisa-ai/shisa-de-2.1"), body)
	m, err := NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2", Readout: r, Calibration: &record})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	report := m.Health(context.Background(), true)
	if report["probe_calibration_applicable"] != false || report["probe_calibration"] != record.ID() || report["probe_calibration_warning"] == nil {
		t.Fatalf("probe %v", report)
	}
	if !m.CalibrationApplied {
		t.Fatal("explicit authorization changed")
	}
}

func TestUnfittedResponseWarningAndLegacyThoughtCoverage(t *testing.T) {
	record, _ := CalibrationFor(FamilyDE2)
	record.Reads = nil
	record.Temperatures = map[string]float64{"noul": 1, "choice": 1}
	m := &DecisionModel{Model: "alias", Family: FamilyDE2, ReadoutVersion: DE2ReadoutVersion, Calibration: record, calibrationSupplied: true}
	a := Answer{ServedModel: "shisa-ai/shisa-de-2.1"}
	m.noteCalibration(&a, record, false)
	if !strings.Contains(a.CalibrationWarning, "no fitted temperature") {
		t.Fatalf("warning %q", a.CalibrationWarning)
	}
	record.Temperatures["noul"] = 2
	m.Calibration = record
	liveAssertCalibration(t, m, Answer{Type: "noul", ServedModel: "shisa-ai/shisa-de-2.1", Strategy: StrategyRepeatThink, Temperature: 1}, "think")
}

func TestBrokenBundledRecordReportsItsCause(t *testing.T) {
	m := &DecisionModel{Model: "alias", Family: FamilyDE2, ReadoutVersion: DE2ReadoutVersion}
	// Simulate a cache load failure without mutating the embedded filesystem or
	// global checkpoint registry.
	m.servedRecordsOnce.Do(func() {
		m.servedRecords = map[string]Calibration{}
		m.servedRecordErrors = map[string]error{"shisa-ai/shisa-de-2.1": fmt.Errorf("invalid bundled JSON")}
	})
	record, applied := m.servedRecord("shisa-ai/shisa-de-2.1", false)
	a := Answer{ServedModel: "shisa-ai/shisa-de-2.1"}
	m.noteCalibration(&a, record, applied)
	if applied || !strings.Contains(a.CalibrationWarning, "invalid bundled JSON") {
		t.Fatalf("applied %v warning %q", applied, a.CalibrationWarning)
	}
}
