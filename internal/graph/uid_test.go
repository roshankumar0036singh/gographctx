package graph

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUIDBasicsAndParsing(t *testing.T) {
	if !ZeroUID.IsZero() {
		t.Fatal("ZeroUID.IsZero() = false, want true")
	}

	u1 := GenerateUID("pkg::Foo", "sig1")
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

	u2 := GenerateUID("pkg::Bar", "sig1")
	if u1 == u2 {
		t.Fatal("distinct QNames produced identical UIDs")
	}
	if u1.Compare(u2) == 0 {
		t.Fatal("Compare different UIDs returned 0")
	}

	// JSON round-trip via MarshalText / UnmarshalText
	raw, err := json.Marshal(u1)
	if err != nil {
		t.Fatalf("json.Marshal(UID) failed: %v", err)
	}
	var decoded UID
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(UID) failed: %v", err)
	}
	if decoded != u1 {
		t.Fatalf("JSON round-trip mismatch: got %v, want %v", decoded, u1)
	}

	// Error cases
	if _, err := ParseUID("too-short"); err == nil {
		t.Fatal("expected error for short UID string")
	}
	if _, err := ParseUID(strings.Repeat("z", 32)); err == nil {
		t.Fatal("expected error for non-hex UID string")
	}
	var bad UID
	if err := bad.UnmarshalText([]byte("bad")); err == nil {
		t.Fatal("expected error from UnmarshalText for invalid input")
	}
}

func TestSignatureHashAndUIDStability(t *testing.T) {
	// Parameter names must NOT alter SignatureHash
	sigA := SignatureHash([]Param{{Name: "a", Type: "int"}, {Name: "b", Type: "string"}}, "bool")
	sigB := SignatureHash([]Param{{Name: "x", Type: "int"}, {Name: "y", Type: "string"}}, "bool")
	if sigA != sigB {
		t.Fatalf("renaming typed params changed signature hash: %s != %s", sigA, sigB)
	}

	// Whitespace trimming around types
	sigTrim := SignatureHash([]Param{{Name: "x", Type: " int "}, {Name: "y", Type: "string "}}, " bool ")
	if sigA != sigTrim {
		t.Fatalf("type whitespace changed signature hash: %s != %s", sigA, sigTrim)
	}

	// Changing parameter type MUST alter SignatureHash
	sigTypeChange := SignatureHash([]Param{{Name: "a", Type: "int64"}, {Name: "b", Type: "string"}}, "bool")
	if sigA == sigTypeChange {
		t.Fatal("changing param type did not change signature hash")
	}

	// Changing return type MUST alter SignatureHash
	sigRetChange := SignatureHash([]Param{{Name: "a", Type: "int"}, {Name: "b", Type: "string"}}, "error")
	if sigA == sigRetChange {
		t.Fatal("changing return type did not change signature hash")
	}

	// Untyped functions: renaming params preserves hash, changing arity changes hash
	untyped2A := SignatureHash([]Param{{Name: "foo"}, {Name: "bar"}}, "")
	untyped2B := SignatureHash([]Param{{Name: "baz"}, {Name: "qux"}}, "")
	untyped3 := SignatureHash([]Param{{Name: "foo"}, {Name: "bar"}, {Name: "extra"}}, "")
	if untyped2A != untyped2B {
		t.Fatal("renaming untyped params changed signature hash")
	}
	if untyped2A == untyped3 {
		t.Fatal("changing untyped arity did not change signature hash")
	}

	// Length-prefix injection resistance: ("ab", "cd") vs ("a", "bcd")
	if GenerateUID("ab", "cd") == GenerateUID("a", "bcd") {
		t.Fatal("length-prefix collision between split inputs")
	}
}

func TestComputeNodeUID(t *testing.T) {
	if !ComputeNodeUID(nil).IsZero() {
		t.Fatal("ComputeNodeUID(nil) should be ZeroUID")
	}

	// Line numbers must NOT affect UID
	fnLine10 := &Node{
		Kind:       KindFunction,
		Name:       "Handle",
		QName:      "pkg::Server::Handle",
		File:       "server.go",
		StartLine:  10,
		EndLine:    25,
		Params:     []Param{{Name: "req", Type: "Request"}},
		ReturnType: "Response",
	}
	fnLine100 := &Node{
		Kind:       KindFunction,
		Name:       "Handle",
		QName:      "pkg::Server::Handle",
		File:       "server.go",
		StartLine:  100,
		EndLine:    115,
		Params:     []Param{{Name: "r", Type: "Request"}},
		ReturnType: "Response",
	}
	if ComputeNodeUID(fnLine10) != ComputeNodeUID(fnLine100) {
		t.Fatal("moving function lines or renaming params changed UID")
	}

	// Overload with different signature gets different UID
	fnOverload := fnLine10.Clone()
	fnOverload.Params = []Param{{Name: "req", Type: "Request"}, {Name: "ctx", Type: "Context"}}
	if ComputeNodeUID(fnLine10) == ComputeNodeUID(fnOverload) {
		t.Fatal("overloaded function with different signature produced same UID")
	}

	// Non-function kinds with same QName remain distinct
	clsNode := &Node{Kind: KindClass, Name: "Config", QName: "app::Config"}
	varNode := &Node{Kind: KindVariable, Name: "Config", QName: "app::Config"}
	if ComputeNodeUID(clsNode) == ComputeNodeUID(varNode) {
		t.Fatal("Class and Variable with same QName should have distinct UIDs")
	}

	// Fallback when QName is empty
	noQNameWithFile := &Node{Kind: KindFunction, Name: "helper", File: "a.go"}
	noQNameNoFile := &Node{Kind: KindFunction, Name: "helper"}
	if ComputeNodeUID(noQNameWithFile).IsZero() || ComputeNodeUID(noQNameNoFile).IsZero() {
		t.Fatal("fallback QName should still produce non-zero UID")
	}
	if ComputeNodeUID(noQNameWithFile) == ComputeNodeUID(noQNameNoFile) {
		t.Fatal("file-scoped fallback UID should differ from fileless fallback UID")
	}
}

