package shisade

import (
	"fmt"
	"strings"
)

// Family is a DE contract. The family decides more than a calibration record:
// DE-1 and DE-2 are read through different contracts, so reading a checkpoint
// through the other family's contract returns answers nobody has measured.
type Family string

const (
	// FamilyDE1 is the letter-slot contract, de1-letter-slots-v3. This client
	// does not implement it.
	FamilyDE1 Family = "de1"
	// FamilyDE2 is the codebook contract, de2-codebook-v1.
	FamilyDE2 Family = "de2"
)

// Families is every family, in the order they were introduced.
var Families = []Family{FamilyDE1, FamilyDE2}

// DefaultFamily is the family assumed when nothing names one. DE-2 is the
// forward line, so an id that says nothing is read as DE-2, and the caller is
// told the family was assumed rather than detected.
const DefaultFamily = FamilyDE2

// FamilySource says where a family came from, so an assumption is never
// presented as a detection.
type FamilySource string

const (
	// SourceDeclared is a family the caller passed.
	SourceDeclared FamilySource = "declared"
	// SourceModelID is a family named by the served model id.
	SourceModelID FamilySource = "model id"
	// SourceTokenizer is a family named by the tokenizer source, for a server
	// that aliases the model id.
	SourceTokenizer FamilySource = "tokenizer"
	// SourceAssumed is DefaultFamily, because nothing named a family.
	SourceAssumed FamilySource = "assumed"
)

// UnsupportedFamilyError reports a checkpoint this client cannot read. It is
// returned instead of falling back to another contract, because the answers
// would be unmeasured.
type UnsupportedFamilyError struct {
	Family Family
	Model  string
	Source FamilySource
}

func (e *UnsupportedFamilyError) Error() string {
	return fmt.Sprintf(
		"shisade: %s is a %s model (%s), and this client implements the %s contract only; "+
			"reading it as %s would return answers nobody has measured",
		e.Model, e.Family, e.Source, FamilyDE2, FamilyDE2)
}

// NormalizeFamily reads "de1", "DE-1", "de_2" and the like as a Family.
func NormalizeFamily(spec string) (Family, error) {
	var builder strings.Builder
	for _, r := range strings.ToLower(spec) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
		}
	}
	name := Family(builder.String())
	for _, family := range Families {
		if family == name {
			return family, nil
		}
	}
	return "", fmt.Errorf("shisade: unknown family %q; expected one of %v", spec, Families)
}

// slugFamily returns the one family a string names, or "" when it names neither
// or both.
//
// A marker is `de1` or `de-1`, case-insensitively, not touching an alphanumeric
// character on either side, so `shisa-ai/shisa-de-1` and `de1-cont-...` name
// DE-1 while `de-13` and `de1x` name nothing, and an id naming both
// (`de2-vs-de1-ablation`) names neither.
func slugFamily(text string) Family {
	lower := strings.ToLower(text)
	namesDE1 := hasFamilySlug(lower, '1')
	namesDE2 := hasFamilySlug(lower, '2')
	switch {
	case namesDE1 == namesDE2:
		return ""
	case namesDE1:
		return FamilyDE1
	default:
		return FamilyDE2
	}
}

func hasFamilySlug(lower string, digit byte) bool {
	for i := 0; i+3 <= len(lower); i++ {
		if lower[i] != 'd' || lower[i+1] != 'e' {
			continue
		}
		at := i + 2
		if at < len(lower) && lower[at] == '-' {
			at++
		}
		if at >= len(lower) || lower[at] != digit {
			continue
		}
		if at+1 < len(lower) && isAlphanumeric(lower[at+1]) {
			continue
		}
		if i > 0 && isAlphanumeric(lower[i-1]) {
			continue
		}
		return true
	}
	return false
}

func isAlphanumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// FamilyIsExplicit reports whether an id names exactly one family, rather than
// leaving it to be assumed.
func FamilyIsExplicit(modelID string) bool {
	return slugFamily(modelID) != ""
}

// ModelFamily is the family an id names, otherwise DefaultFamily.
//
// The default is an assumption, not a detection: use FamilyIsExplicit or
// ResolveFamily when the difference matters.
func ModelFamily(modelID string) Family {
	if named := slugFamily(modelID); named != "" {
		return named
	}
	return DefaultFamily
}

// ResolveFamily reports which contract a served model is read through, and
// where that came from.
//
// The order is: a family the caller declared, then a slug in the served model
// id, then a slug in the tokenizer source, then DefaultFamily as an assumption.
//
// An empty declared string means "not declared", so resolution falls through.
// Python's resolve_family treats family="" as an invalid declaration and raises;
// Go's zero value is the empty string, so "not declared" is the better reading
// of it here. A caller that wants Python's strictness validates the string
// first.
func ResolveFamily(modelID, tokenizer, declared string) (Family, FamilySource, error) {
	if declared != "" {
		family, err := NormalizeFamily(declared)
		if err != nil {
			return "", "", err
		}
		return family, SourceDeclared, nil
	}
	if named := slugFamily(modelID); named != "" {
		return named, SourceModelID, nil
	}
	if named := slugFamily(tokenizer); named != "" {
		return named, SourceTokenizer, nil
	}
	return DefaultFamily, SourceAssumed, nil
}

// RequireDE2 resolves the family and refuses anything but DE-2.
//
// This port implements one contract. A DE-1 checkpoint is an error rather than a
// fallback, because reading it as DE-2 would return answers nobody has measured,
// and there is no DE-1 read here to return instead.
func RequireDE2(modelID, tokenizer, declared string) (FamilySource, error) {
	family, source, err := ResolveFamily(modelID, tokenizer, declared)
	if err != nil {
		return "", err
	}
	if family != FamilyDE2 {
		return source, &UnsupportedFamilyError{Family: family, Model: modelID, Source: source}
	}
	return source, nil
}
