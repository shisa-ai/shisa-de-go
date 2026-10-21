package shisade

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func servedBody(id string) string {
	return strings.Replace(codeReadBody(map[string]float64{"A": -0.1, "B": -3}), `"usage"`, `"model":`+strconvQuote(id)+`,"usage"`, 1)
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestResponseCalibrationSelection(t *testing.T) {
	for _, tc := range []struct {
		name, model, served, version, want string
		applied                            bool
	}{
		{"switch", "shisa-ai/shisa-de-2", "shisa-ai/shisa-de-2.1", "", "shisa-ai/shisa-de-2.1", true},
		{"unknown", "shisa-ai/shisa-de-2", "shisa-ai/shisa-de-2.2", "", "shisa-ai/shisa-de-2", false},
		{"neutral to de2", "neutral", "shisa-ai/shisa-de-2", "", "shisa-ai/shisa-de-2", true},
		{"neutral to de21", "neutral", "shisa-ai/shisa-de-2.1-FP8", "", "shisa-ai/shisa-de-2.1", true},
		{"incompatible", "shisa-ai/shisa-de-2", "shisa-ai/shisa-de-2.1", "incompatible-future-readout", "shisa-ai/shisa-de-2.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readout, _ := newPolicyReadout(t, 2, servedBody(tc.served))
			m, err := NewDecisionModel(DecisionModelConfig{Model: tc.model, Tokenizer: "neutral", Family: FamilyDE2, Readout: readout})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if tc.version != "" {
				m.ReadoutVersion = tc.version
			}
			record, applied := m.servedRecord(tc.served, m.CalibrationApplied)
			if record.Model != tc.want || applied != tc.applied {
				t.Fatalf("record %s applied %v", record.Model, applied)
			}
			d, err := m.Decide(context.Background(), "evidence", map[string]Question{"q": NewNoul("yes?")}, CallOptions{})
			if err != nil {
				t.Fatal(err)
			}
			a := d.Answers["q"]
			if a.Calibrated != tc.applied {
				t.Fatalf("calibrated %v", a.Calibrated)
			}
			if !tc.applied && (a.Calibration != "" || a.Temperature != 1 || a.CalibrationWarning == "") {
				t.Fatalf("raw provenance %+v", a)
			}
			if tc.applied && a.Calibration != record.ID() {
				t.Fatalf("wrong record %s", a.Calibration)
			}
			if a.ToDict()["served_model"] != tc.served || a.ToDict()["calibration"] != a.Calibration {
				t.Fatal("serialized provenance missing")
			}
			if d.Meta["calibration"] != a.Calibration || d.Meta["configured_calibration"] != m.Calibration.ID() {
				t.Fatalf("meta %v", d.Meta)
			}
		})
	}
}

func TestResponseCalibrationMixedHeadsAndMultiLabel(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2, servedBody("shisa-ai/shisa-de-2.1"), servedBody("shisa-ai/shisa-de-2.2"))
	m, err := NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2", Readout: readout, MaxWorkers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	d, err := m.Classify(context.Background(), "evidence", map[string]HeadSpec{"topic": {Options: []Option{{Key: "a"}, {Key: "b"}}, MultiLabel: true}}, ClassifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Meta["calibration"] != nil {
		t.Fatalf("mixed record reported as uniform: %v", d.Meta)
	}
	records := d.Meta["calibration_record_by_head"].(map[string]string)
	if !strings.HasPrefix(records["topic:a"], "shisa-ai/shisa-de-2.1|") || records["topic:b"] != "" {
		t.Fatalf("records %v", records)
	}
	for _, head := range []string{"topic:a", "topic:b"} {
		if _, ok := d.Raw[head]["served_model"]; !ok {
			t.Fatalf("missing %s provenance", head)
		}
	}
	if _, err := json.Marshal(d); err != nil {
		t.Fatal(err)
	}
}

func TestResponseCalibrationCacheIsPrivateAndConcurrent(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2)
	m, err := NewDecisionModel(DecisionModelConfig{Model: "neutral", Family: FamilyDE2, Readout: readout})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record, applied := m.servedRecord("shisa-ai/shisa-de-2", false)
			if !applied || record.TemperatureFor("noul", "direct") != 2.69 {
				t.Error("cache returned wrong record")
			}
		}()
	}
	wg.Wait()
	m.Calibration.Reads["direct"]["noul"] = 99
	record, applied := m.servedRecord("shisa-ai/shisa-de-2", false)
	if !applied || record.TemperatureFor("noul", "direct") != 2.69 {
		t.Fatal("public record mutation leaked into private cache")
	}
}

func TestResponseImageIdentityAndHealth(t *testing.T) {
	body := strings.Replace(chatBody(`{"token":"A","logprob":-0.1},{"token":"B","logprob":-1.0}`), `"usage"`, `"model":"shisa-ai/shisa-de-2.1","usage"`, 1)
	model, _ := newStubModel(t, MaxCodes, body)
	defer model.Close()
	d, err := model.Decide(context.Background(), "evidence", map[string]Question{"q": NewNoul("yes?")}, CallOptions{Image: imageTestURL})
	if err != nil {
		t.Fatal(err)
	}
	a := d.Answers["q"]
	if a.ServedModel != "shisa-ai/shisa-de-2.1" || a.Calibration != "" || a.Calibrated || a.Temperature != 1 {
		t.Fatalf("image %+v", a)
	}
	if a.ToDict()["served_model"] != a.ServedModel || d.Meta["calibration"] != "" {
		t.Fatal("image provenance lost")
	}

	model2, _ := newStubModel(t, MaxCodes, modelsBody("shisa-ai/shisa-de-2"), servedBody("shisa-ai/shisa-de-2.1"), body)
	defer model2.Close()
	report := model2.Health(context.Background(), true)
	if report["probe_served_model"] != "shisa-ai/shisa-de-2.1" || report["probe_calibration_applicable"] != true {
		t.Fatalf("health %v", report)
	}
}

func TestResponseCalibrationExplicitDisagreement(t *testing.T) {
	record, _ := CalibrationFor(FamilyDE2)
	readout, _ := newPolicyReadout(t, 2, servedBody("shisa-ai/shisa-de-2.1"))
	m, err := NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2", Readout: readout, Calibration: &record})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	d, err := m.Decide(context.Background(), "evidence", map[string]Question{"q": NewNoul("yes?")}, CallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a := d.Answers["q"]
	if a.Calibration != record.ID() || a.CalibrationWarning == "" {
		t.Fatalf("explicit override %+v", a)
	}
}
