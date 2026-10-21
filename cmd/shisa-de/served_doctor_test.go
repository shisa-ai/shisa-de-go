package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDoctorDisplaysResponseCalibration(t *testing.T) {
	report := map[string]any{"probe_served_model": "shisa-ai/shisa-de-2.1", "probe_calibration": "served-fit", "probe_calibration_applicable": true, "probe_calibration_warning": "explicit record retained"}
	var out bytes.Buffer
	writeDoctorReport(&out, report)
	for _, text := range []string{"probe model     shisa-ai/shisa-de-2.1", "probe fit       served-fit", "applicable    True", "explicit record retained"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("missing %q in %s", text, out.String())
		}
	}
	ordered := orderedReport(report)
	for key := range report {
		if _, ok := ordered.Get(key); !ok {
			t.Fatalf("JSON report drops %s", key)
		}
	}
}
