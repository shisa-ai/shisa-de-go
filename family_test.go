package shisade

import (
	"errors"
	"testing"
)

func TestSlugFamily(t *testing.T) {
	cases := []struct {
		id   string
		want Family
	}{
		{"shisa-ai/shisa-de-1", FamilyDE1},
		{"shisa-ai/shisa-de-2", FamilyDE2},
		{"de-1", FamilyDE1},
		{"de1", FamilyDE1},
		{"de1-cont-lr1e5", FamilyDE1},
		{"de2-v4-lr5e5-e3-s7", FamilyDE2},
		{"DE-2", FamilyDE2},
		// The slug is `de-?1`, so an underscore is not a marker: only a declared
		// family is normalized that loosely.
		{"my-de_1-finetune", ""},
		{"de-13", ""},
		{"de1x", ""},
		{"xde1", ""},
		{"de2-vs-de1-ablation", ""},
		{"gemma-4-26b-a4b-it", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := slugFamily(tc.id); got != tc.want {
			t.Errorf("slugFamily(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestNormalizeFamily(t *testing.T) {
	for _, spec := range []string{"de1", "DE-1", "de_1", "De 1", "d.e.1"} {
		got, err := NormalizeFamily(spec)
		if err != nil {
			t.Errorf("NormalizeFamily(%q): %v", spec, err)
			continue
		}
		if got != FamilyDE1 {
			t.Errorf("NormalizeFamily(%q) = %q, want de1", spec, got)
		}
	}
	if _, err := NormalizeFamily("de3"); err == nil {
		t.Error("NormalizeFamily(\"de3\") should fail")
	}
}

func TestResolveFamilyOrder(t *testing.T) {
	cases := []struct {
		model, tokenizer, declared string
		want                       Family
		source                     FamilySource
	}{
		// A declaration wins over everything.
		{"shisa-ai/shisa-de-1", "google/gemma-4-26B-A4B-it", "de2", FamilyDE2, SourceDeclared},
		{"shisa-ai/shisa-de-2", "", "de1", FamilyDE1, SourceDeclared},
		// Then the model id.
		{"shisa-ai/shisa-de-2", "shisa-ai/shisa-de-1", "", FamilyDE2, SourceModelID},
		// Then the tokenizer, for a server that aliases the model id.
		{"my-endpoint-alias", "shisa-ai/shisa-de-2", "", FamilyDE2, SourceTokenizer},
		// Otherwise DE-2, as an assumption.
		{"my-endpoint-alias", "google/gemma-4-26B-A4B-it", "", FamilyDE2, SourceAssumed},
		{"", "", "", FamilyDE2, SourceAssumed},
	}
	for _, tc := range cases {
		family, source, err := ResolveFamily(tc.model, tc.tokenizer, tc.declared)
		if err != nil {
			t.Errorf("ResolveFamily(%q, %q, %q): %v", tc.model, tc.tokenizer, tc.declared, err)
			continue
		}
		if family != tc.want || source != tc.source {
			t.Errorf("ResolveFamily(%q, %q, %q) = (%q, %q), want (%q, %q)",
				tc.model, tc.tokenizer, tc.declared, family, source, tc.want, tc.source)
		}
	}
	if _, _, err := ResolveFamily("m", "", "de9"); err == nil {
		t.Error("an unknown declared family should fail")
	}
}

func TestFamilyIsExplicitAndModelFamily(t *testing.T) {
	if !FamilyIsExplicit("shisa-ai/shisa-de-1") {
		t.Error("shisa-ai/shisa-de-1 names a family")
	}
	if FamilyIsExplicit("my-alias") {
		t.Error("my-alias names no family")
	}
	if got := ModelFamily("my-alias"); got != DefaultFamily {
		t.Errorf("ModelFamily(\"my-alias\") = %q, want the default %q", got, DefaultFamily)
	}
	if got := ModelFamily("shisa-ai/shisa-de-1"); got != FamilyDE1 {
		t.Errorf("ModelFamily(\"shisa-ai/shisa-de-1\") = %q, want de1", got)
	}
}

func TestRequireDE2RefusesDE1(t *testing.T) {
	source, err := RequireDE2("shisa-ai/shisa-de-2", "", "")
	if err != nil {
		t.Fatalf("RequireDE2 on DE-2: %v", err)
	}
	if source != SourceModelID {
		t.Errorf("source = %q, want %q", source, SourceModelID)
	}

	_, err = RequireDE2("shisa-ai/shisa-de-1", "", "")
	var unsupported *UnsupportedFamilyError
	if !errors.As(err, &unsupported) {
		t.Fatalf("RequireDE2 on DE-1 returned %v, want *UnsupportedFamilyError", err)
	}
	if unsupported.Family != FamilyDE1 || unsupported.Model != "shisa-ai/shisa-de-1" {
		t.Errorf("error names family %q and model %q", unsupported.Family, unsupported.Model)
	}
}

func TestReadoutVersions(t *testing.T) {
	if got := ReadoutVersionFor(FamilyDE2); got != DE2ReadoutVersion {
		t.Errorf("ReadoutVersionFor(de2) = %q", got)
	}
	if got := ReadoutVersionFor(FamilyDE1); got != DE1ReadoutVersion {
		t.Errorf("ReadoutVersionFor(de1) = %q", got)
	}
	if got := ReadoutVersionFor("de9"); got != "" {
		t.Errorf("ReadoutVersionFor(de9) = %q, want empty", got)
	}
	// DE-2 lists no compatible predecessor: its direct read is its own.
	if CalibrationCompatibleWith("de1-letter-slots-v1", DE2ReadoutVersion) {
		t.Error("a DE-1 version must not be compatible with the DE-2 readout")
	}
	if !CalibrationCompatibleWith(DE2ReadoutVersion, DE2ReadoutVersion) {
		t.Error("a version is compatible with itself")
	}
	if !CalibrationCompatibleWith("de1-letter-slots-v1", DE1ReadoutVersion) {
		t.Error("de1-letter-slots-v1 should be compatible with de1-letter-slots-v3")
	}
	if CalibrationCompatibleWith("de1-letter-slots-v4", DE1ReadoutVersion) {
		t.Error("an unknown predecessor must not be compatible")
	}
}

func TestSoftmax(t *testing.T) {
	probabilities, err := Softmax(map[string]float64{"A": -2.9802276912960224e-06, "B": -13.12500286102295})
	if err != nil {
		t.Fatalf("Softmax: %v", err)
	}
	if got := probabilities["A"]; got < 0.999998 || got > 0.999999 {
		t.Errorf("A = %v, want about 0.999998", got)
	}
	if got := probabilities["B"]; got > 0.0000021 {
		t.Errorf("B = %v, want about 0.000002", got)
	}
	total := probabilities["A"] + probabilities["B"]
	if total < 0.999999999 || total > 1.000000001 {
		t.Errorf("the distribution sums to %v", total)
	}
	if _, err := Softmax(nil); err == nil {
		t.Error("Softmax of nothing should fail")
	}
	var readoutErr *ReadoutError
	if _, err := Softmax(map[string]float64{}); !errors.As(err, &readoutErr) {
		t.Errorf("Softmax of an empty map returned %v, want *ReadoutError", err)
	}
	// A peak that is negative infinity would make every weight NaN; the
	// measured reads never produce one, but the arithmetic should not silently
	// return a distribution that does not sum.
	probabilities, err = Softmax(map[string]float64{"A": -1, "B": -1, "C": -1})
	if err != nil {
		t.Fatalf("Softmax of a tie: %v", err)
	}
	for key, value := range probabilities {
		if value < 0.3333 || value > 0.3334 {
			t.Errorf("a tied code %q = %v, want about 1/3", key, value)
		}
	}
}
