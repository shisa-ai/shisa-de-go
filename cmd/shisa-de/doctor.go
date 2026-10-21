package main

import (
	"context"
	"fmt"
	"io"

	shisade "github.com/shisa-ai/shisa-de-go"
)

// runDoctor is the Python command line's `doctor` branch.
//
// The report is `Health`'s map, and the text form prints the same lines in the
// same order.
func runDoctor(ctx context.Context, args *args, model *shisade.DecisionModel,
	stdout, stderr io.Writer) int {
	report := model.Health(ctx, args.probe)
	// The library records what the Python client would have sent to
	// `warnings.warn`, which writes to stderr. `doctor` is where a caller checks
	// which record is in force, so the warnings belong beside the report: a model
	// id and a tokenizer that name different checkpoints is exactly the state a
	// reader is looking for and cannot see in the fields.
	for _, warning := range model.Warnings {
		fmt.Fprintf(stderr, "shisa-de doctor: %s\n", warning)
	}
	if args.json {
		// `json.dumps(report, indent=2)`, which is `ensure_ascii=True`: the
		// Python command line does not pass the flag, so its default applies and
		// a non-ASCII model id or error message is escaped.
		indent := "  "
		text, err := shisade.PythonJSONWith(orderedReport(report), shisade.PythonJSONOptions{
			Indent: &indent, EnsureASCII: true,
		})
		if err != nil {
			fmt.Fprintf(stderr, "shisa-de doctor: the report is not JSON: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, text)
	} else {
		writeDoctorReport(stdout, report)
	}
	if ok, _ := report["ok"].(bool); !ok {
		return 1
	}
	return 0
}

// reportOrder is the order `Health` inserts the report's keys in, which is the
// order `json.dumps` writes them in.
//
// A Go map has no order, so an unordered report would print its keys sorted and
// `doctor --json` would not be byte-identical to the Python command line's. The
// values are the same either way; this is for the reader who diffs the two.
var reportOrder = []string{
	"base_url", "model", "model_family", "family_source", "family_explicit",
	"readout_version", "policy", "reads", "reasoning", "compound",
	"calibration", "calibration_model", "calibration_readout_version",
	"calibration_serving_shape", "calibration_level", "calibration_applied", "ok",
	"family_note", "calibration_match", "calibration_mismatch", "calibration_note",
	"models_status", "served_models", "model_listed", "slots", "boundary_check",
	"read_probe", "probe_served_model", "probe_calibration", "probe_calibration_applicable",
	"probe_calibration_warning", "chat_system_render",
}

// orderedReport is the report as an `Object` in that order, with the keys the
// report does not carry left out.
func orderedReport(report map[string]any) shisade.Object {
	ordered := make(shisade.Object, 0, len(report))
	for _, key := range reportOrder {
		value, ok := report[key]
		if !ok {
			continue
		}
		ordered = append(ordered, shisade.Member{Key: key, Value: value})
	}
	return ordered
}

// writeDoctorReport is the report as the aligned text block.
func writeDoctorReport(out io.Writer, report map[string]any) {
	fmt.Fprintf(out, "endpoint        %v\n", report["base_url"])
	fmt.Fprintf(out, "model           %v\n", report["model"])
	fmt.Fprintf(out, "family          %v (%v)\n", report["model_family"], report["family_source"])
	fmt.Fprintf(out, "readout         %v, policy %v\n", report["readout_version"], report["policy"])
	fmt.Fprintf(out, "calibration     %v\n", report["calibration"])
	fmt.Fprintf(out, "  fitted on     %v (%v, %v)\n",
		report["calibration_model"], report["calibration_readout_version"],
		report["calibration_serving_shape"])
	// A record that does not match reports `NO` rather than its level, because
	// the level names how far it is from matching and the reader wants the
	// verdict first.
	if matched, _ := report["calibration_match"].(bool); matched {
		fmt.Fprintf(out, "  match         %v\n", report["calibration_level"])
	} else {
		fmt.Fprintf(out, "  match         NO\n")
	}
	if applied, _ := report["calibration_applied"].(bool); applied {
		fmt.Fprintf(out, "  applied       yes\n")
	} else {
		fmt.Fprintf(out, "  applied       no\n")
	}
	for _, reason := range reasonsOf(report, "calibration_mismatch") {
		fmt.Fprintf(out, "                %s\n", reason)
	}
	for _, reason := range reasonsOf(report, "calibration_note") {
		fmt.Fprintf(out, "                %s\n", reason)
	}
	fmt.Fprintf(out, "models endpoint %v\n", report["models_status"])
	if served, ok := report["served_models"].([]string); ok {
		listed := "no"
		if value, _ := report["model_listed"].(bool); value {
			listed = "yes"
		}
		fmt.Fprintf(out, "model listed    %s (of %d served ids)\n", listed, len(served))
	}
	fmt.Fprintf(out, "boundary check  %v\n", report["boundary_check"])
	if probe, ok := report["read_probe"]; ok {
		fmt.Fprintf(out, "read probe      %v\n", probe)
	}
	if served, ok := report["probe_served_model"]; ok {
		fmt.Fprintf(out, "probe model     %v\n", served)
		fmt.Fprintf(out, "probe fit       %v\n", report["probe_calibration"])
		fmt.Fprintf(out, "  applicable    %s\n", pythonBool(report["probe_calibration_applicable"]))
	}
	if warning, ok := report["probe_calibration_warning"]; ok {
		fmt.Fprintf(out, "probe warning   %v\n", warning)
	}
	if system, ok := report["chat_system_render"]; ok {
		fmt.Fprintf(out, "chat system     %v\n", system)
	}
	if note, ok := report["family_note"]; ok {
		fmt.Fprintf(out, "note            %v\n", note)
	}
	fmt.Fprintf(out, "ok              %s\n", pythonBool(report["ok"]))
}

// reasonsOf is a report entry that is a list of strings, or nothing.
func reasonsOf(report map[string]any, key string) []string {
	reasons, _ := report[key].([]string)
	return reasons
}

// pythonBool spells a Go bool the way Python prints one, because the report's
// text form is read next to the Python command line's.
func pythonBool(value any) string {
	flag, _ := value.(bool)
	if flag {
		return "True"
	}
	return "False"
}
