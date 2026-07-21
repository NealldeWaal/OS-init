package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeInventory(t *testing.T, dir string, pkg Package) string {
	t.Helper()
	inv := Inventory{SchemaVersion: 1, Packages: []Package{pkg}}
	b, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	p := filepath.Join(dir, "test-inv.json")
	if err := os.WriteFile(p, b, 0644); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	return p
}

// captureOutput runs f with stdout redirected and returns the captured output.
func captureOutput(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		outC <- buf.String()
	}()
	f()
	_ = w.Close()
	os.Stdout = old
	return <-outC
}

func TestRun_SkipsWhenInstalled(t *testing.T) {
	origRunner := runner
	defer func() { runner = origRunner }()

	fr := &fakeRunner{
		lookPaths: map[string]error{"sw_vers": nil, "brew": nil, "mas": nil},
		outputs:   map[string][]byte{"mas|list": []byte("497799835 Xcode (11.3)\n")},
	}
	runner = fr

	dir := t.TempDir()
	invPath := writeInventory(t, dir, Package{Name: "Xcode", Method: "mac_app_store", ID: "497799835"})

	opt := options{file: invPath, dryRun: true, continueOnError: false, methods: map[string]bool{"mac_app_store": true}}

	out := captureOutput(func() {
		if err := run(opt); err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	})

	if !bytes.Contains([]byte(out), []byte("SKIPPED: Xcode is already installed")) {
		t.Fatalf("expected SKIPPED message, got output: %s", out)
	}
	if bytes.Contains([]byte(out), []byte("$ 'mas' 'install' '497799835'")) {
		t.Fatalf("did not expect install command to be printed when installed, output: %s", out)
	}
}

func TestRun_RunsWhenNotInstalled(t *testing.T) {
	origRunner := runner
	defer func() { runner = origRunner }()

	fr := &fakeRunner{
		lookPaths: map[string]error{"sw_vers": nil, "brew": nil, "mas": nil},
		outputs:   map[string][]byte{"mas|list": []byte("")},
	}
	runner = fr

	dir := t.TempDir()
	invPath := writeInventory(t, dir, Package{Name: "Xcode", Method: "mac_app_store", ID: "497799835"})

	opt := options{file: invPath, dryRun: true, continueOnError: false, methods: map[string]bool{"mac_app_store": true}}

	out := captureOutput(func() {
		if err := run(opt); err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	})

	if !bytes.Contains([]byte(out), []byte("$ 'mas' 'install' '497799835'")) {
		t.Fatalf("expected install command to be printed when not installed, got output: %s", out)
	}
}

func TestRun_InvokesRunContextWhenNotInstalled(t *testing.T) {
	origRunner := runner
	defer func() { runner = origRunner }()

	fr := &fakeRunner{
		lookPaths: map[string]error{"sw_vers": nil, "brew": nil, "mas": nil},
		outputs:   map[string][]byte{"mas|list": []byte("")},
	}
	runner = fr

	dir := t.TempDir()
	invPath := writeInventory(t, dir, Package{Name: "Xcode", Method: "mac_app_store", ID: "497799835"})

	opt := options{file: invPath, dryRun: false, continueOnError: false, methods: map[string]bool{"mac_app_store": true}}

	_ = captureOutput(func() {
		if err := run(opt); err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	})

	found := false
	for _, r := range fr.runs {
		if r == "mas|install|497799835" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected mas install to be invoked via runner.RunContext; runs: %v", fr.runs)
	}
}

func TestRun_DoesNotInvokeRunContextWhenInstalled(t *testing.T) {
	origRunner := runner
	defer func() { runner = origRunner }()

	fr := &fakeRunner{
		lookPaths: map[string]error{"sw_vers": nil, "brew": nil, "mas": nil},
		outputs:   map[string][]byte{"mas|list": []byte("497799835 Xcode (11.3)\n")},
	}
	runner = fr

	dir := t.TempDir()
	invPath := writeInventory(t, dir, Package{Name: "Xcode", Method: "mac_app_store", ID: "497799835"})

	opt := options{file: invPath, dryRun: false, continueOnError: false, methods: map[string]bool{"mac_app_store": true}}

	_ = captureOutput(func() {
		if err := run(opt); err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	})

	for _, r := range fr.runs {
		if r == "mas|install|497799835" {
			t.Fatalf("did not expect mas install to be invoked when package is already installed; runs: %v", fr.runs)
		}
	}
}
