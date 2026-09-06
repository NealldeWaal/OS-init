package main

import (
	"context"
	"errors"
	"testing"
)

// fakeRunner implements Runner for tests. It allows configuring outputs and
// errors for LookPath and OutputContext, and records RunContext calls.
type fakeRunner struct {
	lookPaths map[string]error
	outputs   map[string][]byte // key is name + "|" + joined args
	runs      []string
}

func (f *fakeRunner) LookPath(file string) (string, error) {
	if err, ok := f.lookPaths[file]; ok {
		return "", err
	}
	return "/usr/bin/" + file, nil
}

func key(name string, args ...string) string {
	k := name
	for _, a := range args {
		k += "|" + a
	}
	return k
}

func (f *fakeRunner) OutputContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	if out, ok := f.outputs[key(name, args...)]; ok {
		return out, nil
	}
	return nil, errors.New("no output configured")
}

func (f *fakeRunner) RunContext(ctx context.Context, name string, args ...string) error {
	f.runs = append(f.runs, key(name, args...))
	return nil
}

func TestInstalled_HomebrewFormula(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })

	fr := &fakeRunner{
		lookPaths: map[string]error{"brew": nil},
		outputs:   map[string][]byte{"brew|list|--versions|jq": []byte("jq 1.6")},
	}
	runner = fr

	pkg := Package{Name: "jq", Method: "homebrew_formula", ID: "jq"}
	ok, err := installed(context.Background(), pkg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected installed=true")
	}
}

func TestInstalled_HomebrewFormula_NotInstalled(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })

	fr := &fakeRunner{
		lookPaths: map[string]error{"brew": nil},
		outputs:   map[string][]byte{},
	}
	runner = fr

	pkg := Package{Name: "foo", Method: "homebrew_formula", ID: "foo"}
	ok, err := installed(context.Background(), pkg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected installed=false")
	}
}

func TestInstalled_Mas(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })

	fr := &fakeRunner{
		lookPaths: map[string]error{"mas": nil},
		outputs:   map[string][]byte{"mas|list": []byte("497799835 Xcode (11.3)\n")},
	}
	runner = fr

	pkg := Package{Name: "Xcode", Method: "mac_app_store", ID: "497799835"}
	ok, err := installed(context.Background(), pkg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected installed=true for mas")
	}
}

func TestInstalled_Mas_NotInstalled(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })

	fr := &fakeRunner{
		lookPaths: map[string]error{"mas": nil},
		outputs:   map[string][]byte{"mas|list": []byte("")},
	}
	runner = fr

	pkg := Package{Name: "Xcode", Method: "mac_app_store", ID: "497799835"}
	ok, err := installed(context.Background(), pkg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected installed=false for mas")
	}
}

// setAppIndex pre-seeds the bundle identifier index so tests exercise the
// on-disk fallback without touching the real /Applications directory.
func setAppIndex(t *testing.T, index map[string]appInfo) {
	t.Helper()
	orig := appIndex
	appIndex = index
	t.Cleanup(func() { appIndex = orig })
}

func TestInstalled_HomebrewCask_UnmanagedApp(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })

	// brew has no record of the cask, but the app is on disk.
	runner = &fakeRunner{lookPaths: map[string]error{"brew": nil}, outputs: map[string][]byte{}}
	setAppIndex(t, map[string]appInfo{"com.figma.Desktop": {path: "/Applications/Figma.app", version: "126.8.18"}})

	pkg := Package{Name: "Figma", Method: "homebrew_cask", ID: "figma", BundleID: "com.figma.Desktop"}
	ok, err := installed(context.Background(), pkg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected installed=true for an app present outside Homebrew")
	}
}

func TestInstalled_HomebrewCask_NotInstalled(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })

	runner = &fakeRunner{lookPaths: map[string]error{"brew": nil}, outputs: map[string][]byte{}}
	setAppIndex(t, map[string]appInfo{})

	pkg := Package{Name: "Discord", Method: "homebrew_cask", ID: "discord", BundleID: "com.hnc.Discord"}
	ok, err := installed(context.Background(), pkg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected installed=false when neither brew nor the filesystem knows the cask")
	}
}

func TestInstalled_Mas_FallsBackToBundleWhenMasMissing(t *testing.T) {
	origRunner := runner
	t.Cleanup(func() { runner = origRunner })

	runner = &fakeRunner{lookPaths: map[string]error{"mas": errors.New("not found")}}
	setAppIndex(t, map[string]appInfo{"com.microsoft.Word": {path: "/Applications/Microsoft Word.app", version: "16.112.3"}})

	pkg := Package{Name: "Microsoft Word", Method: "mac_app_store", ID: "462054704", BundleID: "com.microsoft.Word"}
	ok, err := installed(context.Background(), pkg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected installed=true from the bundle fallback when mas is unavailable")
	}
}

func TestAppPath_EmptyBundleIDNeverMatches(t *testing.T) {
	setAppIndex(t, map[string]appInfo{"": {path: "/Applications/Bogus.app"}})

	if _, ok := appPath(context.Background(), ""); ok {
		t.Fatalf("expected an empty bundle identifier to never match")
	}
}
