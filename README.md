# OS-Init

A small Go tool to install macOS applications from an inventory JSON file.
It supports Homebrew formulae and casks, the Mac App Store (mas), and manual
items. The tool is designed to be idempotent: it checks whether items are
already installed and skips them when possible.

## Quickstart

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
- Idempotent checks for Homebrew and mas (skips already-installed packages)
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
    -dry-run
          print commands without running them
    -continue-on-error
          continue installing after a command fails
    -methods string
          comma-separated installation methods (default "homebrew_formula,homebrew_cask,mac_app_store,manual")
```

### Example

Create an inventory file (mac-apps.json):

```JSON
{
  "schema_version": 1,
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

## Notes and behavior

- The tool checks for Homebrew and mas and will attempt to install Homebrew (by downloading the official installer script) and mas (via Homebrew) when needed unless running with -dry-run.
- For mac_app_store entries, the ID must be the numeric MAS app id (mas requires a numeric id). The tool validates this and reports a clear error for non-numeric IDs.
- The idempotence checks use `brew list --versions` and `mas list` to decide whether to skip an install. These checks are best-effort and require the corresponding tools to be available in PATH.
- The tool prints skipped/failed items and returns a non-zero status when installs fail. With `-continue-on-error`, it attempts the remaining packages before returning the failure.

## Contributing

- Run `go test ./...` and `go vet ./...` before submitting changes.
- Add unit tests for new behavior; shelling out commands may need to be mocked via refactoring.

## License

This repository has no license file by default. Add one if you intend to share it publicly.
