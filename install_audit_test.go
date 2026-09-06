package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_ReportsEveryProblemAtOnce(t *testing.T) {
	inv := Inventory{
		SchemaVersion: schemaVersion,
		Packages: []Package{
			{Name: "good", Method: "homebrew_formula", ID: "jq"},
			{Name: "bad method", Method: "snap", ID: "thing"},
			{Name: "no id", Method: "homebrew_cask", ID: "  "},
			{Name: "bad mas id", Method: "mac_app_store", ID: "com.example.app"},
			{Name: "dupe", Method: "homebrew_formula", ID: "jq"},
		},
	}

	err := validate(inv)
	if err == nil {
		t.Fatalf("expected validation to fail")
	}
	for _, want := range []string{"unknown method", "has no id", "non-numeric", "repeats the id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q; got:\n%s", want, err)
		}
	}
}

func TestValidate_RejectsDuplicateBundleID(t *testing.T) {
	inv := Inventory{
		SchemaVersion: schemaVersion,
		Packages: []Package{
			{Name: "one", Method: "homebrew_cask", ID: "a", BundleID: "com.example.app"},
			{Name: "two", Method: "homebrew_cask", ID: "b", BundleID: "com.example.app"},
		},
	}
	if err := validate(inv); err == nil || !strings.Contains(err.Error(), "repeats the bundle_id") {
		t.Fatalf("expected duplicate bundle_id to be rejected, got %v", err)
	}
}

func TestValidate_AcceptsTheRealInventory(t *testing.T) {
	if _, err := readInventory(APPS); err != nil {
		t.Fatalf("the shipped inventory should validate: %v", err)
	}
}

func TestReadInventory_RejectsUnknownField(t *testing.T) {
	// A misspelled key must fail loudly: silently decoding it as absent is what
	// disables on-disk detection without anyone noticing.
	body := `{"schema_version":1,"packages":[
		{"name":"Figma","method":"homebrew_cask","id":"figma","bundle-id":"com.figma.Desktop"}]}`
	p := filepath.Join(t.TempDir(), "inv.json")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := readInventory(p)
	if err == nil || !strings.Contains(err.Error(), "bundle-id") {
		t.Fatalf("expected the misspelled key to be rejected, got %v", err)
	}
}

func TestBundleVersion(t *testing.T) {
	cases := []struct {
		name, short, build, want string
	}{
		{"marketing version wins", "1.2.3", "4567", "1.2.3"},
		// R reports a sentence as its short version and a build counter as its
		// version; neither is usable, so nothing is reported.
		{"descriptive short version and opaque build", "R R 4.6.1 GUI 1.83 High Sierra build", "8608", ""},
		{"falls back to a dotted build", "", "3.1.4f1", "3.1.4f1"},
		{"rejects an undotted build", "", "8608", ""},
		{"nothing available", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bundleVersion(c.short, c.build); got != c.want {
				t.Fatalf("bundleVersion(%q, %q) = %q, want %q", c.short, c.build, got, c.want)
			}
		})
	}
}

func TestFormulaKey(t *testing.T) {
	if got := formulaKey("lizardbyte/homebrew/sunshine"); got != "sunshine" {
		t.Fatalf("expected the bare formula name, got %q", got)
	}
	if got := formulaKey("htop"); got != "htop" {
		t.Fatalf("expected an untapped name to pass through, got %q", got)
	}
}

func TestMasVersions_ParsesListOutput(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })
	runner = &fakeRunner{
		lookPaths: map[string]error{"mas": nil},
		outputs:   map[string][]byte{"mas|list": []byte("497799835  Xcode  (26.6)\n441258766  Magnet  (3.0.7)\n")},
	}

	got := masVersions(context.Background())
	if got["497799835"] != "26.6" || got["441258766"] != "3.0.7" {
		t.Fatalf("unexpected parse: %#v", got)
	}
}

func TestMasVersions_EmptyWhenMasUnavailable(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })
	runner = &fakeRunner{lookPaths: map[string]error{"mas": errors.New("not found")}}

	if got := masVersions(context.Background()); len(got) != 0 {
		t.Fatalf("expected no versions when mas is missing, got %#v", got)
	}
}

func TestAudit_ReportsMissingDriftAndUnlisted(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })
	runner = &fakeRunner{
		lookPaths: map[string]error{"mas": errors.New("not found")},
		outputs: map[string][]byte{
			"brew|list|--versions":        []byte("gnupg 2.5.21\nripgrep 15.2.0\n"),
			"brew|list|--cask|--versions": []byte("figma 126.8.18\nngrok 3.39.11\n"),
			"brew|leaves":                 []byte("gnupg\nripgrep\n"),
		},
	}
	setAppIndex(t, map[string]appInfo{
		"com.figma.Desktop": {path: "/Applications/Figma.app", version: "126.8.18"},
		"md.obsidian":       {path: "/Applications/Obsidian.app", version: "1.12.7"},
	})

	inv := Inventory{
		SchemaVersion: schemaVersion,
		Packages: []Package{
			{Name: "GnuPG", Method: "homebrew_formula", ID: "gnupg", ObservedVersion: "2.5.20"},
			{Name: "Figma", Method: "homebrew_cask", ID: "figma", BundleID: "com.figma.Desktop", ObservedVersion: "126.8.18"},
			{Name: "Discord", Method: "homebrew_cask", ID: "discord", BundleID: "com.hnc.Discord"},
		},
		Excluded: []Excluded{{Name: "ripgrep", Reason: "not wanted", IDs: []string{"ripgrep"}}},
	}

	rep := audit(context.Background(), inv)

	if rep.present != 2 || rep.total != 3 {
		t.Fatalf("expected 2 of 3 present, got %d of %d", rep.present, rep.total)
	}
	if len(rep.missing) != 1 || rep.missing[0].Name != "Discord" {
		t.Fatalf("expected Discord to be the only missing package, got %#v", rep.missing)
	}
	if len(rep.drifted) != 1 || rep.drifted[0].recorded != "2.5.20" || rep.drifted[0].actual != "2.5.21" {
		t.Fatalf("expected GnuPG version drift, got %#v", rep.drifted)
	}

	var kinds []string
	for _, item := range rep.unlisted {
		kinds = append(kinds, item.kind+":"+item.id)
	}
	// ripgrep is excluded by id, Figma is listed; ngrok and Obsidian are not.
	want := map[string]bool{"cask:ngrok": true, "app:md.obsidian": true}
	if len(kinds) != len(want) {
		t.Fatalf("unexpected unlisted items: %v", kinds)
	}
	for _, k := range kinds {
		if !want[k] {
			t.Fatalf("unexpected unlisted item %q (all: %v)", k, kinds)
		}
	}
}

func TestRun_AuditChangesNothing(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })
	fr := &fakeRunner{
		lookPaths: map[string]error{"sw_vers": nil, "brew": errors.New("not found"), "mas": errors.New("not found")},
	}
	runner = fr
	setAppIndex(t, map[string]appInfo{})

	inv := Inventory{SchemaVersion: schemaVersion, Packages: []Package{{Name: "Xcode", Method: "mac_app_store", ID: "497799835"}}}
	b, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "inv.json")
	if err := os.WriteFile(p, b, 0644); err != nil {
		t.Fatal(err)
	}

	opt := options{file: p, audit: true, methods: parseMethods("mac_app_store")}
	out := captureOutput(func() {
		if err := run(opt); err != nil {
			t.Fatalf("audit returned error: %v", err)
		}
	})

	// Homebrew is missing, but an audit must not bootstrap or install anything.
	if len(fr.runs) != 0 {
		t.Fatalf("audit must not run commands, got %v", fr.runs)
	}
	if !strings.Contains(out, "Missing (1)") {
		t.Fatalf("expected the missing package to be reported, got:\n%s", out)
	}
}
