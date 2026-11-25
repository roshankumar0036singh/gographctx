package graph

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUIDAndSigHashBasicsAndParsing(t *testing.T) {
	if !ZeroUID.IsZero() {
		t.Fatal("ZeroUID.IsZero() = false, want true")
	}
	if !ZeroSigHash.IsZero() {
		t.Fatal("ZeroSigHash.IsZero() = false, want true")
	}

	u1 := GenerateNodeUID(KindFunction, "pkg::Foo", 0)
	if u1.IsZero() {
		t.Fatal("generated UID should not be zero")
	}
	if len(u1.String()) != 32 {
		t.Fatalf("len(String()) = %d, want 32", len(u1.String()))
	}
	if len(u1.Short()) != 8 || !strings.HasPrefix(u1.String(), u1.Short()) {
		t.Fatalf("Short() = %q not 8-char prefix of %q", u1.Short(), u1.String())
	}

	parsed, err := ParseUID(u1.String())
	if err != nil {
		t.Fatalf("ParseUID failed: %v", err)
	}
	if parsed != u1 {
		t.Fatalf("ParseUID mismatch: got %v, want %v", parsed, u1)
	}
	if u1.Compare(parsed) != 0 {
		t.Fatalf("Compare equal UIDs = %d, want 0", u1.Compare(parsed))
	}

	u2 := GenerateNodeUID(KindFunction, "pkg::Bar", 0)
	if u1 == u2 {
		t.Fatal("distinct QNames produced identical UIDs")
	}
	if u1.Compare(u2) == 0 {
		t.Fatal("Compare different UIDs returned 0")
	}

	// JSON round-trip for UID and SigHash
	raw, err := json.Marshal(u1)
	if err != nil {
		t.Fatalf("json.Marshal(UID) failed: %v", err)
	}
	var decoded UID
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(UID) failed: %v", err)
	}
	if decoded != u1 {
		t.Fatalf("UID JSON round-trip mismatch: got %v, want %v", decoded, u1)
	}

	sig := ComputeSignatureHash([]Param{{Name: "a", Type: "int"}}, "error")
	if sig.IsZero() || len(sig.String()) != 32 {
		t.Fatalf("unexpected SigHash: %v", sig)
	}
	sigRaw, err := json.Marshal(sig)
	if err != nil {
		t.Fatalf("json.Marshal(SigHash) failed: %v", err)
	}
	var decodedSig SigHash
	if err := json.Unmarshal(sigRaw, &decodedSig); err != nil {
		t.Fatalf("json.Unmarshal(SigHash) failed: %v", err)
	}
	if decodedSig != sig {
		t.Fatalf("SigHash JSON round-trip mismatch: got %v, want %v", decodedSig, sig)
	}

	// Error cases
	if _, err := ParseUID("too-short"); err == nil {
		t.Fatal("expected error for short UID string")
	}
	if _, err := ParseUID(strings.Repeat("z", 32)); err == nil {
		t.Fatal("expected error for non-hex UID string")
	}
	var badUID UID
	if err := badUID.UnmarshalText([]byte("bad")); err == nil {
		t.Fatal("expected error from UID.UnmarshalText for invalid input")
	}
	var badSig SigHash
	if err := badSig.UnmarshalText([]byte("short")); err == nil {
		t.Fatal("expected error from SigHash.UnmarshalText for short input")
	}
	if err := badSig.UnmarshalText([]byte(strings.Repeat("g", 32))); err == nil {
		t.Fatal("expected error from SigHash.UnmarshalText for invalid hex")
	}
}

func TestComputeSignatureHash(t *testing.T) {
	// Parameter names must NOT alter SignatureHash
	sigA := ComputeSignatureHash([]Param{{Name: "a", Type: "int"}, {Name: "b", Type: "string"}}, "bool")
	sigB := ComputeSignatureHash([]Param{{Name: "x", Type: "int"}, {Name: "y", Type: "string"}}, "bool")
	if sigA != sigB {
		t.Fatalf("renaming typed params changed signature hash: %s != %s", sigA, sigB)
	}

	// Whitespace trimming around types
	sigTrim := ComputeSignatureHash([]Param{{Name: "x", Type: " int "}, {Name: "y", Type: "string "}}, " bool ")
	if sigA != sigTrim {
		t.Fatalf("type whitespace changed signature hash: %s != %s", sigA, sigTrim)
	}

	// Changing parameter type MUST alter SignatureHash
	sigTypeChange := ComputeSignatureHash([]Param{{Name: "a", Type: "int64"}, {Name: "b", Type: "string"}}, "bool")
	if sigA == sigTypeChange {
		t.Fatal("changing param type did not change signature hash")
	}

	// Changing return type MUST alter SignatureHash
	sigRetChange := ComputeSignatureHash([]Param{{Name: "a", Type: "int"}, {Name: "b", Type: "string"}}, "error")
	if sigA == sigRetChange {
		t.Fatal("changing return type did not change signature hash")
	}

	// Untyped functions: renaming params preserves hash, changing arity changes hash
	untyped2A := ComputeSignatureHash([]Param{{Name: "foo"}, {Name: "bar"}}, "")
	untyped2B := ComputeSignatureHash([]Param{{Name: "baz"}, {Name: "qux"}}, "")
	untyped3 := ComputeSignatureHash([]Param{{Name: "foo"}, {Name: "bar"}, {Name: "extra"}}, "")
	if untyped2A != untyped2B {
		t.Fatal("renaming untyped params changed signature hash")
	}
	if untyped2A == untyped3 {
		t.Fatal("changing untyped arity did not change signature hash")
	}

	// Length-prefix injection resistance: ("a", "bc") vs ("ab", "c")
	sigSplit1 := ComputeSignatureHash([]Param{{Type: "a"}}, "bc")
	sigSplit2 := ComputeSignatureHash([]Param{{Type: "ab"}}, "c")
	if sigSplit1 == sigSplit2 {
		t.Fatal("length-prefix collision between split signature inputs")
	}
}

func TestComputeNodeUIDStabilityAndOccurrence(t *testing.T) {
	// Zero / invalid Node
	if !ComputeNodeUID(Node{}).IsZero() {
		t.Fatal("ComputeNodeUID(empty) should be ZeroUID")
	}
	if !ComputeNodeUID(Node{Kind: KindUnknown, QName: "pkg::X"}).IsZero() {
		t.Fatal("ComputeNodeUID(KindUnknown) should be ZeroUID")
	}

	// Non-File node without QName must NOT fall back to File::Name
	noQName := Node{Kind: KindFunction, Name: "__init__", File: "app.py", StartLine: 10}
	if !ComputeNodeUID(noQName).IsZero() {
		t.Fatal("ComputeNodeUID for non-File node with empty QName should be ZeroUID")
	}

	// KindFile falls back to File when QName is empty
	fileFallback := Node{Kind: KindFile, Name: "app.py", File: "pkg/app.py"}
	if ComputeNodeUID(fileFallback).IsZero() {
		t.Fatal("KindFile should compute UID from File when QName is empty")
	}

	// Moving lines OR adding parameter type annotations preserves UID while changing SignatureHash
	beforeEdit := Node{
		Kind:          KindFunction,
		Name:          "f",
		QName:         "mod::f",
		File:          "mod.py",
		StartLine:     10,
		EndLine:       15,
		Params:        []Param{{Name: "a"}},
		SignatureHash: ComputeSignatureHash([]Param{{Name: "a"}}, ""),
	}
	afterTypeAnnotation := Node{
		Kind:          KindFunction,
		Name:          "f",
		QName:         "mod::f",
		File:          "mod.py",
		StartLine:     42,
		EndLine:       50,
		Params:        []Param{{Name: "a", Type: "int"}},
		ReturnType:    "str",
		SignatureHash: ComputeSignatureHash([]Param{{Name: "a", Type: "int"}}, "str"),
	}
	if ComputeNodeUID(beforeEdit) != ComputeNodeUID(afterTypeAnnotation) {
		t.Fatal("adding type annotations or moving lines changed node UID")
	}
	if beforeEdit.SignatureHash == afterTypeAnnotation.SignatureHash {
		t.Fatal("adding type annotations did not change SignatureHash")
	}

	// Same QName siblings (e.g. Go package init() functions or Python duplicate __init__)
	// are disambiguated cleanly by Occurrence.
	init0 := Node{Kind: KindFunction, Name: "init", QName: "pkg::init", Occurrence: 0, File: "a.go", StartLine: 5}
	init1 := Node{Kind: KindFunction, Name: "init", QName: "pkg::init", Occurrence: 1, File: "b.go", StartLine: 5}
	if ComputeNodeUID(init0) == ComputeNodeUID(init1) {
		t.Fatal("distinct Occurrence values produced identical UIDs")
	}

	// Non-function kinds with same QName remain distinct
	clsNode := Node{Kind: KindClass, Name: "Config", QName: "app::Config"}
	varNode := Node{Kind: KindVariable, Name: "Config", QName: "app::Config"}
	if ComputeNodeUID(clsNode) == ComputeNodeUID(varNode) {
		t.Fatal("Class and Variable with same QName should have distinct UIDs")
	}

	// KindExternal produces valid UID
	ext := Node{Kind: KindExternal, Name: "Println", QName: "fmt.Println"}
	if ComputeNodeUID(ext).IsZero() {
		t.Fatal("KindExternal should produce valid non-zero UID")
	}
}
