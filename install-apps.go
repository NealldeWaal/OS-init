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
	"runtime"
	"sort"
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

// notesEnabled controls advisory NOTE output; -quiet turns it off. It is a
// package-level switch for the same reason runner is: the functions that emit
// notes sit well below the code that parses flags.
var notesEnabled = true

// note reports something worth knowing that is not a problem. Notes join the
// run narrative on stdout rather than the timestamped error log, so redirecting
// stdout captures the whole story of a run and stderr carries only trouble.
func note(format string, args ...any) {
	if !notesEnabled {
		return
	}
	fmt.Printf("NOTE: "+format+"\n", args...)
}

// Inventory describes the top-level JSON structure that drives installs.
// SchemaVersion guards against incompatible inventory versions, Packages is the
// ordered list of packages to process, and Excluded records what was left out
// on purpose.
type Inventory struct {
	SchemaVersion int        `json:"schema_version"`
	GeneratedAt   string     `json:"generated_at"`
	Source        string     `json:"source"`
	Packages      []Package  `json:"packages"`
	Excluded      []Excluded `json:"excluded"`
}

// Excluded records something deliberately left out of Packages, so an audit can
// tell "we decided against this" apart from "we forgot this". BundleIDs lets the
// audit match the exclusion against what is actually on disk; without it the
// entry is documentation only.
type Excluded struct {
	Name      string   `json:"name"`
	Reason    string   `json:"reason"`
	IDs       []string `json:"ids"`
	BundleIDs []string `json:"bundle_ids"`
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
	audit           bool
	quiet           bool
	methods         map[string]bool
}

// APPS is the default path to the JSON inventory file.
const APPS = "mac-apps.json"

// schemaVersion is the inventory format this build understands. Version 2
// dropped the unused "bootstrap" and "installers" keys, which described
// behavior that has always been hardcoded in run().
const schemaVersion = 2

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
	flag.BoolVar(&opt.audit, "audit", false, "report how the inventory compares with what is installed, then exit")
	flag.BoolVar(&opt.quiet, "quiet", false, "suppress advisory NOTE output")

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
	if err := validateMethods(opt.methods); err != nil {
		return err
	}
	notesEnabled = !opt.quiet

	inv, err := readInventory(opt.file)
	if err != nil {
		return err
	}
	warnMissingBundleIDs(inv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// An audit reports on every entry regardless of -methods, and never
	// bootstraps Homebrew or mas: it must not change the machine it inspects.
	if opt.audit {
		printAudit(opt.file, audit(ctx, inv))
		return nil
	}

	needsBrew := opt.methods["homebrew_formula"] || opt.methods["homebrew_cask"] || opt.methods["mac_app_store"]
	if needsBrew {
		if _, err := runner.LookPath("brew"); err != nil {
			fmt.Println("==> Installing Homebrew")
			if err := installHomebrew(ctx, opt.dryRun); err != nil {
				return err
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
		note("mas installs only apps already associated with the signed-in Apple ID; sign in to the App Store first")
	}

	total, failed, manual := 0, 0, 0
	for _, pkg := range inv.Packages {
		if !opt.methods[pkg.Method] {
			continue
		}
		total++
		fmt.Printf("\n==> %s [%s]\n", pkg.Name, pkg.Method)

		if pkg.Method == "manual" {
			if path, ok := appPath(ctx, pkg.BundleID); ok {
				fmt.Printf("SKIPPED: %s is already installed at %s\n", pkg.Name, path)
				continue
			}
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
			// Treated like a command failure so -continue-on-error applies:
			// one unusable entry should not strand the rest of the inventory.
			failed++
			log.Printf("FAILED: %s: %v", pkg.Name, err)
			if !opt.continueOnError {
				return fmt.Errorf("installation stopped after %s failed", pkg.Name)
			}
			continue
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

// auditReport is the result of comparing the inventory against the machine.
type auditReport struct {
	total    int
	present  int
	missing  []Package
	drifted  []versionDrift
	unlisted []unlistedItem
}

// versionDrift is a package whose recorded observed_version no longer matches
// what is installed.
type versionDrift struct {
	pkg      Package
	recorded string
	actual   string
}

// unlistedItem is something installed that the inventory does not account for.
type unlistedItem struct {
	kind string
	id   string
	name string
}

// audit compares the inventory with what is installed, changing nothing. It
// queries Homebrew and mas in bulk rather than per package: an audit touches
// every entry, so a handful of commands replace one per package.
func audit(ctx context.Context, inv Inventory) auditReport {
	formulae := brewVersions(ctx, false)
	casks := brewVersions(ctx, true)
	masApps := masVersions(ctx)

	listedFormulae := make(map[string]bool)
	listedCasks := make(map[string]bool)
	listedBundles := make(map[string]bool)

	rep := auditReport{total: len(inv.Packages)}
	for _, pkg := range inv.Packages {
		switch pkg.Method {
		case "homebrew_formula":
			listedFormulae[formulaKey(pkg.ID)] = true
		case "homebrew_cask":
			listedCasks[pkg.ID] = true
		}
		if pkg.BundleID != "" {
			listedBundles[pkg.BundleID] = true
		}

		actual, present := packageState(ctx, pkg, formulae, casks, masApps)
		if !present {
			rep.missing = append(rep.missing, pkg)
			continue
		}
		rep.present++
		if actual != "" && pkg.ObservedVersion != "" && actual != pkg.ObservedVersion {
			rep.drifted = append(rep.drifted, versionDrift{pkg: pkg, recorded: pkg.ObservedVersion, actual: actual})
		}
	}

	// A deliberate exclusion is accounted for, so it should not be reported as
	// something the inventory forgot.
	for _, ex := range inv.Excluded {
		for _, id := range ex.BundleIDs {
			listedBundles[id] = true
		}
		for _, id := range ex.IDs {
			listedFormulae[formulaKey(id)] = true
			listedCasks[id] = true
		}
	}
	rep.unlisted = unlistedItems(ctx, listedFormulae, listedCasks, listedBundles, casks)
	return rep
}

// packageState reports the installed version of a package and whether it is
// installed at all. The version can be empty for something that is installed but
// reports no usable version, so the boolean is the authority, not the string.
func packageState(ctx context.Context, pkg Package, formulae, casks, masApps map[string]string) (string, bool) {
	switch pkg.Method {
	case "homebrew_formula":
		if v, ok := formulae[formulaKey(pkg.ID)]; ok {
			return v, true
		}
	case "homebrew_cask":
		if v, ok := casks[pkg.ID]; ok {
			return v, true
		}
	case "mac_app_store":
		if v, ok := masApps[pkg.ID]; ok {
			// The bundle carries the more precise version when it is readable.
			if info, found := lookupApp(ctx, pkg.BundleID); found && info.version != "" {
				return info.version, true
			}
			return v, true
		}
	}
	// Fall back to the bundle on disk for everything the package managers do not
	// account for: manual entries, apps installed by hand, and App Store apps
	// when mas is unavailable.
	if info, ok := lookupApp(ctx, pkg.BundleID); ok {
		return info.version, true
	}
	return "", false
}

// unlistedItems finds things installed on the machine that the inventory does
// not mention. Only Homebrew leaves are considered, since listing every
// dependency would bury the entries a human actually chose to install.
func unlistedItems(ctx context.Context, listedFormulae, listedCasks, listedBundles map[string]bool, casks map[string]string) []unlistedItem {
	var items []unlistedItem

	if out, err := runner.OutputContext(ctx, "brew", "leaves"); err == nil {
		for _, leaf := range strings.Fields(string(out)) {
			if !listedFormulae[formulaKey(leaf)] {
				items = append(items, unlistedItem{kind: "formula", id: leaf})
			}
		}
	}

	caskIDs := make([]string, 0, len(casks))
	for id := range casks {
		caskIDs = append(caskIDs, id)
	}
	sort.Strings(caskIDs)
	for _, id := range caskIDs {
		if !listedCasks[id] {
			items = append(items, unlistedItem{kind: "cask", id: id})
		}
	}

	if appIndex == nil {
		appIndex = buildAppIndex(ctx)
	}
	bundleIDs := make([]string, 0, len(appIndex))
	for id := range appIndex {
		bundleIDs = append(bundleIDs, id)
	}
	sort.Strings(bundleIDs)
	for _, id := range bundleIDs {
		if listedBundles[id] {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(appIndex[id].path), ".app")
		items = append(items, unlistedItem{kind: "app", id: id, name: name})
	}
	return items
}

// formulaKey reduces a possibly tap-qualified formula id to the bare name that
// `brew list` and `brew leaves` report for an installed formula.
func formulaKey(id string) string {
	return id[strings.LastIndex(id, "/")+1:]
}

// brewVersions returns the installed formulae or casks mapped to their versions.
// A formula may report several versions at once, in which case they are kept
// together as brew printed them.
func brewVersions(ctx context.Context, cask bool) map[string]string {
	args := []string{"list", "--versions"}
	if cask {
		args = []string{"list", "--cask", "--versions"}
	}
	versions := make(map[string]string)
	out, err := runner.OutputContext(ctx, "brew", args...)
	if err != nil {
		return versions
	}
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 {
			versions[fields[0]] = strings.Join(fields[1:], " ")
		}
	}
	return versions
}

// masVersions returns App Store apps mapped to the version mas reports. An empty
// map means mas could not answer, not that nothing is installed; callers fall
// back to the bundle on disk.
func masVersions(ctx context.Context) map[string]string {
	versions := make(map[string]string)
	if _, err := runner.LookPath("mas"); err != nil {
		return versions
	}
	out, err := runner.OutputContext(ctx, "mas", "list")
	if err != nil {
		return versions
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		version := ""
		// mas prints "<id> <name> (<version>)"; the version is the last field.
		if last := fields[len(fields)-1]; strings.HasPrefix(last, "(") && strings.HasSuffix(last, ")") {
			version = strings.Trim(last, "()")
		}
		versions[fields[0]] = version
	}
	return versions
}

// printAudit writes the report in the same shape as the install output: what is
// accounted for, what is missing, and what the inventory has not caught up with.
func printAudit(file string, rep auditReport) {
	fmt.Printf("==> Audit of %s\n\n", file)
	fmt.Printf("%d of %d packages installed\n", rep.present, rep.total)

	fmt.Printf("\nMissing (%d):\n", len(rep.missing))
	for _, pkg := range rep.missing {
		fmt.Printf("  - %s [%s: %s]\n", pkg.Name, pkg.Method, pkg.ID)
	}

	fmt.Printf("\nVersion drift (%d):\n", len(rep.drifted))
	for _, d := range rep.drifted {
		fmt.Printf("  - %s: recorded %s, installed %s\n", d.pkg.Name, d.recorded, d.actual)
	}

	fmt.Printf("\nInstalled but not in the inventory (%d):\n", len(rep.unlisted))
	for _, item := range rep.unlisted {
		if item.name != "" {
			fmt.Printf("  - [%s] %s (%s)\n", item.kind, item.name, item.id)
			continue
		}
		fmt.Printf("  - [%s] %s\n", item.kind, item.id)
	}
}

// installHomebrew downloads the official installer and runs it. The script goes
// to a private temporary file rather than a fixed name in the shared temp
// directory: this is the one place the tool executes downloaded code, so the
// path it executes must not be one another user can pre-create or replace.
func installHomebrew(ctx context.Context, dryRun bool) error {
	const installerURL = "https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh"

	if dryRun {
		// Nothing is downloaded or executed, so show a representative path
		// rather than creating a file the run will not use.
		script := filepath.Join(os.TempDir(), "homebrew-install.sh")
		if err := command(ctx, true, "curl", "-fsSL", "-o", script, installerURL); err != nil {
			return err
		}
		return command(ctx, true, "bash", script)
	}

	f, err := os.CreateTemp("", "homebrew-install-*.sh")
	if err != nil {
		return fmt.Errorf("create installer temp file: %w", err)
	}
	script := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close installer temp file: %w", err)
	}
	defer func() { _ = os.Remove(script) }()

	if err := command(ctx, false, "curl", "-fsSL", "-o", script, installerURL); err != nil {
		return fmt.Errorf("failed to download Homebrew install script: %w", err)
	}
	if err := os.Chmod(script, 0o700); err != nil {
		return fmt.Errorf("failed to chmod homebrew installer: %w", err)
	}
	if err := command(ctx, false, "bash", script); err != nil {
		return fmt.Errorf("failed to run Homebrew installer: %w", err)
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
	// Unknown keys are rejected rather than dropped: a misspelled "bundle_id"
	// would otherwise decode to an empty string and silently disable the
	// on-disk detection that keeps installs idempotent.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&inv); err != nil {
		return Inventory{}, fmt.Errorf("decode inventory: %w", err)
	}
	if len(inv.Packages) == 0 {
		return Inventory{}, errors.New("inventory contains no packages")
	}
	if err := validate(inv); err != nil {
		return Inventory{}, err
	}
	return inv, nil
}

// knownMethods lists the installation methods the tool understands.
var knownMethods = []string{"homebrew_formula", "homebrew_cask", "mac_app_store", "manual"}

func isKnownMethod(method string) bool {
	for _, known := range knownMethods {
		if method == known {
			return true
		}
	}
	return false
}

// validate reports inventory problems that would otherwise surface late or not
// at all: an unknown method aborts partway through a run, and a duplicated
// entry installs the same thing twice. Every problem is collected so a single
// run reports all of them rather than one per fix.
func validate(inv Inventory) error {
	var problems []string
	problem := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if inv.SchemaVersion != schemaVersion {
		problem("unsupported schema_version %d (want %d)", inv.SchemaVersion, schemaVersion)
	}

	seenID := make(map[string]int)
	seenBundle := make(map[string]int)
	for i, pkg := range inv.Packages {
		where := fmt.Sprintf("package %d (%q)", i, pkg.Name)
		if strings.TrimSpace(pkg.Name) == "" {
			problem("package %d has no name", i)
		}
		if !isKnownMethod(pkg.Method) {
			problem("%s has unknown method %q (want one of %s)", where, pkg.Method, strings.Join(knownMethods, ", "))
			continue
		}
		if strings.TrimSpace(pkg.ID) == "" {
			problem("%s has no id", where)
			continue
		}
		if pkg.Method == "mac_app_store" {
			if _, err := strconv.Atoi(pkg.ID); err != nil {
				problem("%s has non-numeric mac_app_store id %q", where, pkg.ID)
			}
		}
		if prev, dup := seenID[pkg.Method+" "+pkg.ID]; dup {
			problem("%s repeats the id of package %d", where, prev)
		} else {
			seenID[pkg.Method+" "+pkg.ID] = i
		}
		if pkg.BundleID != "" {
			if prev, dup := seenBundle[pkg.BundleID]; dup {
				problem("%s repeats the bundle_id of package %d", where, prev)
			} else {
				seenBundle[pkg.BundleID] = i
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid inventory:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// warnMissingBundleIDs flags app entries that cannot take part in on-disk
// detection. This is a warning, not an error: the entry still installs, it just
// cannot be recognised when the app is already present by other means.
func warnMissingBundleIDs(inv Inventory) {
	for _, pkg := range inv.Packages {
		if pkg.Type == "app" && strings.TrimSpace(pkg.BundleID) == "" {
			log.Printf("WARNING: %s has no bundle_id, so it cannot be detected if installed outside %s", pkg.Name, pkg.Method)
		}
	}
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

// validateMethods rejects unknown -methods values. Without this a typo such as
// "homebrew_casks" matches no package, and the run reports success having
// installed nothing at all.
func validateMethods(methods map[string]bool) error {
	if len(methods) == 0 {
		return fmt.Errorf("no installation methods selected (want any of %s)", strings.Join(knownMethods, ", "))
	}
	var unknown []string
	for method := range methods {
		if !isKnownMethod(method) {
			unknown = append(unknown, method)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("unknown -methods value %s (want any of %s)",
		strings.Join(unknown, ", "), strings.Join(knownMethods, ", "))
}

// requireMacOS verifies the program is running on macOS. The build target is
// the actual condition; probing PATH for a macOS utility only approximated it.
func requireMacOS() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("this installer must run on macOS (running on %s)", runtime.GOOS)
	}
	return nil
}

// appInfo describes an installed application bundle.
type appInfo struct {
	path    string
	version string
}

// appIndex caches a bundle identifier -> appInfo map so the filesystem scan runs
// at most once per invocation. A nil map means the scan has not run yet; tests
// may pre-seed it.
var appIndex map[string]appInfo

// appSearchDirs lists the directories scanned for installed applications, in
// the order macOS itself uses.
func appSearchDirs() []string {
	dirs := []string{"/Applications", "/Applications/Utilities"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	return dirs
}

// buildAppIndex scans the application directories and maps each bundle
// identifier to the app it belongs to. Unreadable bundles are skipped.
func buildAppIndex(ctx context.Context) map[string]appInfo {
	index := make(map[string]appInfo)
	for _, dir := range appSearchDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".app") {
				continue
			}
			app := filepath.Join(dir, entry.Name())
			plist := filepath.Join(app, "Contents", "Info")
			id := plistValue(ctx, plist, "CFBundleIdentifier")
			if id == "" {
				continue
			}
			index[id] = appInfo{
				path: app,
				version: bundleVersion(
					plistValue(ctx, plist, "CFBundleShortVersionString"),
					plistValue(ctx, plist, "CFBundleVersion"),
				),
			}
		}
	}
	return index
}

// plistValue reads one key from an Info.plist. Bundles store it as either XML or
// Apple's binary plist format, so `defaults` does the reading rather than the
// file being parsed directly. A missing or unreadable key yields "".
func plistValue(ctx context.Context, plist, key string) string {
	out, err := runner.OutputContext(ctx, "defaults", "read", plist, key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// bundleVersion picks the most version-like string a bundle offers.
// CFBundleShortVersionString is the marketing version and is preferred, but some
// bundles put a sentence there instead: R reports "R R 4.6.1 GUI 1.83 High
// Sierra build". CFBundleVersion is the fallback, and only when it looks like a
// dotted version rather than an opaque build counter (R reports "8608"). When
// neither qualifies the version is left unknown rather than guessed.
func bundleVersion(short, build string) string {
	versionLike := func(s string) bool {
		return s != "" && s[0] >= '0' && s[0] <= '9'
	}
	if versionLike(short) {
		return short
	}
	if versionLike(build) && strings.Contains(build, ".") {
		return build
	}
	return ""
}

// lookupApp returns the indexed bundle for the given identifier, building the
// index on first use.
func lookupApp(ctx context.Context, bundleID string) (appInfo, bool) {
	if strings.TrimSpace(bundleID) == "" {
		return appInfo{}, false
	}
	if appIndex == nil {
		appIndex = buildAppIndex(ctx)
	}
	info, ok := appIndex[bundleID]
	return info, ok
}

// appPath returns the path of an installed application with the given bundle
// identifier. It returns false when the identifier is empty or no matching app
// is present.
func appPath(ctx context.Context, bundleID string) (string, bool) {
	info, ok := lookupApp(ctx, bundleID)
	return info.path, ok
}

// appPresent reports whether an app matching the package's bundle identifier is
// already on disk, and is the fallback for the two cases a package manager
// cannot answer for itself: an app that was dragged into place or installed by
// a vendor package (brew has no record of it, yet `brew install --cask` still
// refuses to overwrite it), and an App Store app that cannot be confirmed
// because mas is unavailable. Either way the app is there, so installing again
// is wrong. The match is logged since it is weaker evidence than the package
// manager's own answer: it confirms the app exists, not where it came from.
func appPresent(ctx context.Context, pkg Package) bool {
	path, ok := appPath(ctx, pkg.BundleID)
	if !ok {
		return false
	}
	note("%s found at %s by bundle identifier; %s could not confirm it", pkg.Name, path, pkg.Method)
	return true
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
				return appPresent(ctx, pkg), nil
			}
			if len(bytes.TrimSpace(out)) == 0 {
				return appPresent(ctx, pkg), nil
			}
			return false, fmt.Errorf("brew list --cask failed: %w", err)
		}
		if len(bytes.TrimSpace(out)) == 0 {
			return appPresent(ctx, pkg), nil
		}
		return true, nil
	case "mac_app_store":
		// `mas list` is authoritative when it is available. Without it every
		// App Store package would look uninstalled, so fall back to the bundle
		// identifier on disk when mas is missing or cannot be queried.
		if _, err := runner.LookPath("mas"); err != nil {
			return appPresent(ctx, pkg), nil
		}
		out, err := runner.OutputContext(ctx, "mas", "list")
		if err != nil {
			return appPresent(ctx, pkg), nil
		}
		if parseMasListOutput(string(out), pkg.ID) {
			return true, nil
		}
		return appPresent(ctx, pkg), nil
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
