package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Runner abstracts command execution and lookup so it can be mocked in tests.
// The realRunner wraps the standard os/exec behavior.
type Runner interface {
	LookPath(file string) (string, error)
	OutputContext(ctx context.Context, name string, args ...string) ([]byte, error)
	RunContext(ctx context.Context, name string, args ...string) error
}

// realRunner is the production implementation of Runner using exec.CommandContext.
type realRunner struct{}

func (r realRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }
func (r realRunner) OutputContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}
func (r realRunner) RunContext(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runner is the global command runner used by package functions. Tests may
// override it with a fake implementation.
var runner Runner = realRunner{}

// Inventory describes the top-level JSON structure that drives installs.
// SchemaVersion is used to guard against incompatible inventory versions.
// Bootstrap contains any preparatory steps; Installers is a map of helper
// installers (not currently used); Packages is the ordered list of packages
// to process.
type Inventory struct {
	SchemaVersion int               `json:"schema_version"`
	Bootstrap     []Bootstrap       `json:"bootstrap"`
	Installers    map[string]string `json:"installers"`
	Packages      []Package         `json:"packages"`
}

// Bootstrap represents a preparatory step that may be run before package
// processing. Each bootstrap item can include a check command and the
// corresponding install command or notes for humans.
type Bootstrap struct {
	Name    string `json:"name"`
	Check   string `json:"check"`
	Install string `json:"install"`
	Notes   string `json:"notes"`
}

// Package represents a single item in the inventory to be installed.
// Name is a human-friendly label, Method selects the installer (homebrew_formula,
// homebrew_cask, mac_app_store, manual), and ID is the installer-specific
// identifier (formula name, cask name, numeric mas id, or bundle id for manual).
type Package struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	Method          string `json:"method"`
	ID              string `json:"id"`
	BundleID        string `json:"bundle_id"`
	ObservedVersion string `json:"observed_version"`
	Notes           string `json:"notes"`
}

// options holds runtime flags parsed from the command line.
// file: path to JSON inventory. dryRun: print commands only. continueOnError:
// proceed despite errors. methods: set of enabled install methods.
type options struct {
	file            string
	dryRun          bool
	continueOnError bool
	methods         map[string]bool
}

// APPS is the default path to the JSON inventory file.
const APPS = "mac-apps.user.json"

// main parses command-line flags and invokes run with the provided options.
// Supported flags:
//
//	-file: path to the inventory JSON (default: APPS)
//	-dry-run: print commands without executing them
//	-continue-on-error: proceed when an install command fails
//	-methods: comma-separated list of methods to include
func main() {
	var methodList string

	opt := options{}
	flag.StringVar(&opt.file, "file", APPS, "path to the application inventory JSON")
	flag.BoolVar(&opt.dryRun, "dry-run", false, "print commands without running them")
	flag.BoolVar(&opt.continueOnError, "continue-on-error", false, "continue installing after a command fails")

	flag.StringVar(&methodList, "methods", "homebrew_formula,homebrew_cask,mac_app_store,manual", "comma-separated installation methods")
	flag.Parse()

	opt.methods = parseMethods(methodList)
	if err := run(opt); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

// run is the main orchestration function. It validates platform and inventory,
// bootstraps helpers (Homebrew / mas) when needed, then iterates the inventory
// and installs packages in order. It respects options such as dry-run and
// continueOnError.
func run(opt options) error {
	if err := requireMacOS(); err != nil {
		return err
	}

	inv, err := readInventory(opt.file)
	if err != nil {
		return err
	}
	if inv.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d", inv.SchemaVersion)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	needsBrew := opt.methods["homebrew_formula"] || opt.methods["homebrew_cask"] || opt.methods["mac_app_store"]
	if needsBrew {
		if _, err := runner.LookPath("brew"); err != nil {
			fmt.Println("==> Installing Homebrew")
			// Safer: download the installer script to a temporary file, then execute it
			tmp := filepath.Join(os.TempDir(), "homebrew-install.sh")
			if err := command(ctx, opt.dryRun, "curl", "-fsSL", "-o", tmp, "https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh"); err != nil {
				return fmt.Errorf("failed to download Homebrew install script: %w", err)
			}
			if !opt.dryRun {
				if err := os.Chmod(tmp, 0700); err != nil {
					return fmt.Errorf("failed to chmod homebrew installer: %w", err)
				}
			}
			if err := command(ctx, opt.dryRun, "bash", tmp); err != nil {
				// attempt to remove the temp file even if the installer failed
				if !opt.dryRun {
					_ = os.Remove(tmp)
				}
				return fmt.Errorf("failed to run Homebrew installer: %w", err)
			}
			if !opt.dryRun {
				_ = os.Remove(tmp)
			}
		}
	}

	if opt.methods["mac_app_store"] {
		if _, err := runner.LookPath("mas"); err != nil {
			fmt.Println("==> Installing the Mac App Store CLI (mas)")
			if err := command(ctx, opt.dryRun, "brew", "install", "mas"); err != nil {
				return err
			}
		}
	}

	total, failed, manual := 0, 0, 0
	for _, pkg := range inv.Packages {
		if !opt.methods[pkg.Method] {
			continue
		}
		total++
		fmt.Printf("\n==> %s [%s]\n", pkg.Name, pkg.Method)

		if pkg.Method == "manual" {
			manual++
			fmt.Printf("MANUAL: bundle/package identifier %q", pkg.ID)
			if pkg.Notes != "" {
				fmt.Printf(" — %s", pkg.Notes)
			}
			fmt.Println()
			continue
		}

		// If already installed, skip the install step (idempotence)
		installedFlag, err := installed(ctx, pkg)
		if err != nil {
			// If checking installation failed, treat as not installed but report the error
			log.Printf("WARNING: failed to check installed state for %s: %v", pkg.Name, err)
		} else if installedFlag {
			fmt.Printf("SKIPPED: %s is already installed\n", pkg.Name)
			continue
		}

		name, args, err := installCommand(pkg)
		if err != nil {
			return err
		}
		if err := command(ctx, opt.dryRun, name, args...); err != nil {
			failed++
			log.Printf("FAILED: %s: %v", pkg.Name, err)
			if !opt.continueOnError {
				return fmt.Errorf("installation stopped after %s failed", pkg.Name)
			}
		}
	}

	fmt.Printf("\nProcessed %d packages: %d command failures, %d manual installs.\n", total, failed, manual)
	if failed > 0 {
		return fmt.Errorf("%d installation command(s) failed", failed)
	}
	return nil
}

// readInventory reads the inventory JSON from path and decodes it into an
// Inventory struct. It limits the read to 10MiB for safety and validates that
// at least one package is present.
func readInventory(path string) (Inventory, error) {
	f, err := os.Open(path)
	if err != nil {
		return Inventory{}, fmt.Errorf("open inventory: %w", err)
	}
	defer f.Close()

	var inv Inventory
	dec := json.NewDecoder(io.LimitReader(f, 10<<20))
	if err := dec.Decode(&inv); err != nil {
		return Inventory{}, fmt.Errorf("decode inventory: %w", err)
	}
	if len(inv.Packages) == 0 {
		return Inventory{}, errors.New("inventory contains no packages")
	}
	return inv, nil
}

// installCommand returns the executable name and args for the given package
// based on its Method. It validates required fields (for example mas ids must
// be numeric) and returns a descriptive error for unsupported or malformed
// packages.
func installCommand(pkg Package) (string, []string, error) {
	if strings.TrimSpace(pkg.ID) == "" {
		return "", nil, fmt.Errorf("package %q has no id", pkg.Name)
	}
	switch pkg.Method {
	case "homebrew_formula":
		return "brew", []string{"install", pkg.ID}, nil
	case "homebrew_cask":
		return "brew", []string{"install", "--cask", pkg.ID}, nil
	case "mac_app_store":
		// mas requires numeric application IDs; validate early to provide a clear error
		if _, err := strconv.Atoi(pkg.ID); err != nil {
			return "", nil, fmt.Errorf("package %q has non-numeric mac_app_store id %q: %w", pkg.Name, pkg.ID, err)
		}
		return "mas", []string{"install", pkg.ID}, nil
	default:
		return "", nil, fmt.Errorf("package %q has unsupported method %q", pkg.Name, pkg.Method)
	}
}

// command prints the command to run and executes it unless dryRun is set.
// It uses exec.CommandContext so cancellation via the provided context stops
// the child process when the main program is interrupted or times out.
func command(ctx context.Context, dryRun bool, name string, args ...string) error {
	fmt.Printf("$ %s\n", shellDisplay(name, args))
	if dryRun {
		return nil
	}

	// Delegate execution to the runner to allow tests to inject a fake runner.
	return runner.RunContext(ctx, name, args...)
}

// shellDisplay builds a shell-quoted representation of the command for
// human-readable output. It does not attempt to produce a perfectly re-parsable
// shell line but ensures arguments with spaces and quotes are visibly quoted.
func shellDisplay(name string, args []string) string {
	parts := append([]string{name}, args...)
	for i, part := range parts {
		parts[i] = "'" + strings.ReplaceAll(part, "'", "'\\''") + "'"
	}
	return strings.Join(parts, " ")
}

// parseMethods parses a comma-separated list of method names into a set.
// Empty items are ignored. The returned map is convenient for membership checks
// during package filtering.
func parseMethods(value string) map[string]bool {
	methods := make(map[string]bool)
	for _, method := range strings.Split(value, ",") {
		if method = strings.TrimSpace(method); method != "" {
			methods[method] = true
		}
	}
	return methods
}

// requireMacOS verifies the program is running on macOS by checking for the
// presence of the sw_vers utility in PATH. It returns an error when not on macOS.
func requireMacOS() error {
	if _, err := runner.LookPath("sw_vers"); err != nil {
		return errors.New("this installer must run on macOS")
	}
	return nil
}

// installed checks whether the given package is already installed on the system.
// For Homebrew formulae and casks it uses `brew list --versions` and for mas it
// checks `mas list` output. The function uses a short timeout so checks don't
// hang indefinitely.
func installed(parentCtx context.Context, pkg Package) (bool, error) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	switch pkg.Method {
	case "homebrew_formula":
		out, err := runner.OutputContext(ctx, "brew", "list", "--versions", pkg.ID)
		if err != nil {
			// If command exited non-zero but produced no output, treat as not installed
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) == 0 && len(bytes.TrimSpace(out)) == 0 {
				return false, nil
			}
			// Some brew errors are expected when not installed; if there's no useful output, consider not installed
			if len(bytes.TrimSpace(out)) == 0 {
				return false, nil
			}
			return false, fmt.Errorf("brew list failed: %w", err)
		}
		if len(bytes.TrimSpace(out)) == 0 {
			return false, nil
		}
		return true, nil
	case "homebrew_cask":
		out, err := runner.OutputContext(ctx, "brew", "list", "--cask", "--versions", pkg.ID)
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) == 0 && len(bytes.TrimSpace(out)) == 0 {
				return false, nil
			}
			if len(bytes.TrimSpace(out)) == 0 {
				return false, nil
			}
			return false, fmt.Errorf("brew list --cask failed: %w", err)
		}
		if len(bytes.TrimSpace(out)) == 0 {
			return false, nil
		}
		return true, nil
	case "mac_app_store":
		// Check mas presence first
		if _, err := runner.LookPath("mas"); err != nil {
			return false, nil
		}
		out, err := runner.OutputContext(ctx, "mas", "list")
		if err != nil {
			// If mas list failed, assume not installed rather than erroring the run
			return false, nil
		}
		return parseMasListOutput(string(out), pkg.ID), nil
	default:
		return false, fmt.Errorf("cannot check installed state for unsupported method %q", pkg.Method)
	}
}

// parseMasListOutput returns true if mas list output contains the given numeric id
func parseMasListOutput(output, id string) bool {
	if strings.TrimSpace(output) == "" {
		return false
	}
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == id {
			return true
		}
	}
	return false
}
