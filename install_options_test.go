package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateMethods(t *testing.T) {
	cases := []struct {
		name    string
		methods string
		wantErr string
	}{
		{"all defaults", "homebrew_formula,homebrew_cask,mac_app_store,manual", ""},
		{"single method", "manual", ""},
		// A typo used to match nothing and report success having installed nothing.
		{"typo", "homebrew_casks", "unknown -methods value homebrew_casks"},
		{"one bad among good", "manual,snap", "unknown -methods value snap"},
		{"empty", "", "no installation methods selected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateMethods(parseMethods(c.methods))
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("expected no error, got %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("expected error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestRun_RejectsUnknownMethod(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })
	runner = &fakeRunner{}

	opt := options{file: APPS, dryRun: true, methods: parseMethods("homebrew_casks")}
	err := run(opt)
	if err == nil || !strings.Contains(err.Error(), "unknown -methods value") {
		t.Fatalf("expected the run to be rejected before doing anything, got %v", err)
	}
}

func TestNote_SuppressedWhenQuiet(t *testing.T) {
	orig := notesEnabled
	t.Cleanup(func() { notesEnabled = orig })

	notesEnabled = true
	if out := captureOutput(func() { note("something %d", 1) }); out != "NOTE: something 1\n" {
		t.Fatalf("unexpected note output: %q", out)
	}

	notesEnabled = false
	if out := captureOutput(func() { note("something %d", 1) }); out != "" {
		t.Fatalf("expected no output when quiet, got %q", out)
	}
}

func TestInstallHomebrew_UsesAPrivateTempFileAndCleansUp(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })
	fr := &fakeRunner{}
	runner = fr

	_ = captureOutput(func() {
		if err := installHomebrew(context.Background(), false); err != nil {
			t.Fatalf("installHomebrew: %v", err)
		}
	})

	var script string
	for _, r := range fr.runs {
		if fields := strings.Split(r, "|"); fields[0] == "curl" {
			script = fields[3] // curl|-fsSL|-o|<script>|<url>
		}
	}
	if script == "" {
		t.Fatalf("expected the installer to be downloaded; runs: %v", fr.runs)
	}

	// A fixed name in the shared temp directory is what another user could
	// pre-create or replace with a symlink before it is executed.
	if predictable := filepath.Join(os.TempDir(), "homebrew-install.sh"); script == predictable {
		t.Fatalf("installer path %q is predictable", script)
	}
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Fatalf("expected %q to be removed after the run, stat err: %v", script, err)
	}
}

func TestInstallHomebrew_DryRunExecutesNothing(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })
	fr := &fakeRunner{}
	runner = fr

	out := captureOutput(func() {
		if err := installHomebrew(context.Background(), true); err != nil {
			t.Fatalf("installHomebrew: %v", err)
		}
	})

	if len(fr.runs) != 0 {
		t.Fatalf("a dry run must not execute anything, got %v", fr.runs)
	}
	for _, want := range []string{"curl", "bash"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected the dry run to print the %s step, got:\n%s", want, out)
		}
	}
}

func TestRequireMacOS(t *testing.T) {
	// The check reads runtime.GOOS, so on a macOS runner it must pass; CI pins
	// macos-latest for exactly this reason.
	if err := requireMacOS(); err != nil {
		t.Fatalf("expected macOS to be accepted, got %v", err)
	}
}
