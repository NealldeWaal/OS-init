package main

import (
	"reflect"
	"testing"
)

func TestDefaultInventory(t *testing.T) {
	if APPS != "mac-apps.json" {
		t.Fatalf("expected default inventory %q, got %q", "mac-apps.json", APPS)
	}

	inv, err := readInventory(APPS)
	if err != nil {
		t.Fatalf("read default inventory: %v", err)
	}
	if inv.SchemaVersion != 1 {
		t.Fatalf("expected schema version 1, got %d", inv.SchemaVersion)
	}
}

func TestInstallCommand_MASNumeric(t *testing.T) {
	pkg := Package{Name: "Test MAS", Method: "mac_app_store", ID: "123456"}
	name, args, err := installCommand(pkg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if name != "mas" {
		t.Fatalf("expected name mas, got %q", name)
	}
	if len(args) != 2 || args[0] != "install" || args[1] != pkg.ID {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestInstallCommand_MASNonNumeric(t *testing.T) {
	pkg := Package{Name: "Test MAS", Method: "mac_app_store", ID: "com.example.app"}
	_, _, err := installCommand(pkg)
	if err == nil {
		t.Fatalf("expected error for non-numeric mas id, got nil")
	}
}

func TestParseMethods(t *testing.T) {
	m := parseMethods("a,b,, c")
	expected := map[string]bool{"a": true, "b": true, "c": true}
	if !reflect.DeepEqual(m, expected) {
		t.Fatalf("unexpected parseMethods result: %#v", m)
	}
}

func TestParseMasListOutput(t *testing.T) {
	sample := "497799835 Xcode (11.3)\n409203825 Keynote (10.0)\n"
	if !parseMasListOutput(sample, "497799835") {
		t.Fatalf("expected id 497799835 to be found")
	}
	if parseMasListOutput(sample, "000000") {
		t.Fatalf("did not expect id 000000 to be found")
	}
}
