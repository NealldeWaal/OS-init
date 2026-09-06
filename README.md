# OS-Init

A small Go tool to install macOS applications from an inventory JSON file.
It supports Homebrew formulae and casks, the Mac App Store (mas), and manual
items. The tool is designed to be idempotent: it checks whether items are
already installed and skips them when possible.

## Quickstart

```shell
go run . -audit
```

```shell
go run . -dry-run
```

```shell
go run . -continue-on-error
```

## Features

- Inventory-driven install: specify packages in a JSON file and the tool will process them in order.
- Supports methods: homebrew_formula, homebrew_cask, mac_app_store, manual
- Dry-run mode prints commands without executing them
- Idempotent checks for Homebrew and mas, with a bundle-identifier fallback that also detects hand-installed apps (skips already-installed packages)
- Audit mode reports drift between the inventory and the machine without changing anything
- The inventory is validated on load: unknown keys, unknown methods, malformed Mac App Store ids and duplicate entries are rejected before any install runs
- Safer Homebrew bootstrap: downloads the installer script to a temp file and runs it, avoiding `curl | bash` piping

## Build

Requires Go 1.26.5+.

```shell
go build ./...
```

## Usage

By default the tool reads mac-apps.json in the current directory. Flags:

```shell
    -file string
          path to the application inventory JSON (default "mac-apps.json")
    -audit
          report how the inventory compares with what is installed, then exit
    -dry-run
          print commands without running them
    -continue-on-error
          continue installing after a command fails
    -quiet
          suppress advisory NOTE output
    -methods string
          comma-separated installation methods (default "homebrew_formula,homebrew_cask,mac_app_store,manual")
```

Unknown `-methods` values are rejected. A typo would otherwise match no package
and the run would report success having installed nothing.

### Example

Create an inventory file (mac-apps.json):

```JSON
{
  "schema_version": 2,
  "packages": [
    {"name": "jq", "method": "homebrew_formula", "id": "jq"},
    {"name": "Google Chrome", "method": "homebrew_cask", "id": "google-chrome"},
    {"name": "Xcode", "method": "mac_app_store", "id": "497799835"},
    {"name": "My App (manual)", "method": "manual", "id": "com.example.myapp", "notes": "Download from vendor site"}
  ]
}
```

Then run (dry-run first to verify):

```shell
./os-init -file mac-apps.json -dry-run
```

## Auditing the inventory

An inventory rots: apps get installed by hand, versions move on, and entries
quietly stop matching the machine. `-audit` reports that drift and changes
nothing — it never bootstraps Homebrew or mas, and it ignores `-methods` so the
whole inventory is covered.

```shell
go run . -audit
```

It reports three things:

- **Missing** — listed but not installed.
- **Version drift** — installed at a version other than the recorded `observed_version`.
- **Installed but not in the inventory** — Homebrew leaves, casks and applications
  the inventory does not account for. Homebrew dependencies are ignored, since only
  packages someone chose to install are interesting.

A healthy inventory reports zero in the last two sections. To keep the third one
useful, record deliberate omissions under `excluded` rather than leaving them
out: `ids` suppresses a formula or cask, `bundle_ids` suppresses an application.

```JSON
{
  "name": "docker (formula)",
  "reason": "the docker CLI comes from the Docker Desktop cask",
  "ids": ["docker"]
}
```

## Inventory validation

The inventory is validated when it loads, and every problem found is reported at
once rather than one per run. Unknown JSON keys are an error: a misspelled
`bundle_id` would otherwise decode as absent and silently disable the on-disk
detection described below. Also rejected: unknown `method` values, missing ids,
non-numeric `mac_app_store` ids, and duplicate `id` or `bundle_id` entries.
An `app` entry with no `bundle_id` is a warning, not an error — it still
installs, it just cannot be detected once present.

## Notes and behavior

- The tool checks for Homebrew and mas and will attempt to install Homebrew (by downloading the official installer script) and mas (via Homebrew) when needed unless running with -dry-run.
- For mac_app_store entries, the ID must be the numeric MAS app id (mas requires a numeric id). The tool validates this and reports a clear error for non-numeric IDs.
- `mas` installs only apps already associated with the signed-in Apple ID, so sign in to the App Store before a run that includes `mac_app_store`. On a machine that has never bought a given app, that entry will fail rather than install.
- The idempotence checks use `brew list --versions` and `mas list` to decide whether to skip an install. When those come back empty — because the app was dragged into `/Applications`, installed by a vendor package, or because `mas` is not available — the tool falls back to matching the package's `bundle_id` against the apps in `/Applications`, `/Applications/Utilities`, and `~/Applications`. This matters for casks in particular: Homebrew refuses to install over an app it does not own, so without the fallback every hand-installed app would fail on each run. Fallback matches are logged with a `NOTE:` line, because they confirm only that the app exists, not where it came from.
- `manual` entries are also matched by `bundle_id` and reported as already installed rather than listed as outstanding work.
- Give every `app` entry a `bundle_id`. Without one there is nothing to match on and the fallback cannot run.
- The tool prints skipped/failed items and returns a non-zero status when installs fail. With `-continue-on-error`, it attempts the remaining packages before returning the failure.
- Output is split by kind: the run narrative — commands, `SKIPPED`, `MANUAL`, `NOTE` and the summary — goes to stdout, and only warnings and failures go to stderr. Redirecting stdout captures a complete record of a run; a quiet stderr means nothing went wrong. `-quiet` drops the `NOTE` lines, which are numerous on a machine with many hand-installed apps.
- Homebrew's installer is downloaded to a private temporary file created with `os.CreateTemp` and removed afterwards. This is the only place the tool executes downloaded code, so the path it executes must not be one another user can pre-create or replace.
- The platform check reads `runtime.GOOS`; the tool refuses to run anywhere but macOS.

## Inventory schema

`schema_version` is 2. Version 2 removed the `bootstrap` and `installers` keys:
both described behavior that has always been hardcoded in `run()`, so editing
them changed nothing. A version 1 file is rejected with a message saying so
rather than being read under the newer rules.

Top-level keys: `schema_version`, `generated_at`, `source`, `packages`,
`excluded`. Unknown keys are an error.

## Contributing

- Run `go test ./...` and `go vet ./...` before submitting changes.
- Add unit tests for new behavior; shelling out commands may need to be mocked via refactoring.

## License

[MIT](LICENSE).
