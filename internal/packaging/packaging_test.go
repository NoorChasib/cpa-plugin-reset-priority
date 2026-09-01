package packaging

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/plugin"
)

func TestPluginIDMatchesStoreRules(t *testing.T) {
	if err := ValidatePluginID(plugin.PluginID); err != nil {
		t.Fatalf("plugin id invalid: %v", err)
	}
	for _, bad := range []string{"", "-leading", ".leading", "has space", "has/slash", strings.Repeat("x", 129)} {
		if err := ValidatePluginID(bad); err == nil {
			t.Errorf("ValidatePluginID(%q) accepted", bad)
		}
	}
}

func TestArchiveNamesMatchStoreExpectations(t *testing.T) {
	// Spec section 28.2 exact release asset names.
	want := map[[2]string]string{
		{"linux", "amd64"}:   "reset-priority_0.1.0_linux_amd64.zip",
		{"linux", "arm64"}:   "reset-priority_0.1.0_linux_arm64.zip",
		{"darwin", "amd64"}:  "reset-priority_0.1.0_darwin_amd64.zip",
		{"darwin", "arm64"}:  "reset-priority_0.1.0_darwin_arm64.zip",
		{"windows", "amd64"}: "reset-priority_0.1.0_windows_amd64.zip",
	}
	for platform, wantName := range want {
		got, err := ArchiveName(plugin.PluginID, plugin.PluginVersion, platform[0], platform[1])
		if err != nil {
			t.Fatalf("ArchiveName(%v): %v", platform, err)
		}
		if got != wantName {
			t.Errorf("ArchiveName(%v) = %s, want %s", platform, got, wantName)
		}
	}
	if _, err := ArchiveName(plugin.PluginID, "v0.1.0", "linux", "amd64"); err == nil {
		t.Errorf("ArchiveName accepted a version with leading v")
	}
}

func TestLibraryBasenames(t *testing.T) {
	// Spec section 28.2 exact ZIP-root library names.
	cases := map[string]string{"linux": "reset-priority.so", "darwin": "reset-priority.dylib", "windows": "reset-priority.dll", "freebsd": "reset-priority.so"}
	for goos, want := range cases {
		got, err := LibraryBasename(plugin.PluginID, goos)
		if err != nil {
			t.Fatalf("LibraryBasename(%s): %v", goos, err)
		}
		if got != want {
			t.Errorf("LibraryBasename(%s) = %s, want %s", goos, got, want)
		}
	}
	if _, err := LibraryBasename(plugin.PluginID, "plan9"); err == nil {
		t.Errorf("unsupported GOOS accepted")
	}
}

func TestParseChecksums(t *testing.T) {
	sums, err := ParseChecksums(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  reset-priority_0.1.0_linux_amd64.zip\n" +
			"ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789  reset-priority_0.1.0_darwin_arm64.zip\n\n")
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	if len(sums) != 2 {
		t.Fatalf("sums = %d, want 2", len(sums))
	}
	if sums["reset-priority_0.1.0_linux_amd64.zip"] == "" {
		t.Errorf("linux archive checksum missing")
	}

	// Path-prefixed names break the host's bare-asset-name lookup.
	if _, err := ParseChecksums("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  ./reset-priority_0.1.0_linux_amd64.zip"); err == nil {
		t.Errorf("path-prefixed checksum entry accepted")
	}
	if _, err := ParseChecksums("nothex  file.zip"); err == nil {
		t.Errorf("non-hex checksum accepted")
	}
	if _, err := ParseChecksums("0123 file.zip extra"); err == nil {
		t.Errorf("malformed line accepted")
	}
}

func TestValidateRegistryShape(t *testing.T) {
	good := `{
		"schema_version": 1,
		"plugins": [{
			"id": "reset-priority",
			"name": "Reset Priority",
			"description": "Prioritizes Claude and Codex OAuth accounts by weekly reset time.",
			"author": "NoorChasib",
			"repository": "https://github.com/NoorChasib/cpa-plugin-reset-priority",
			"license": "MIT",
			"tags": ["routing", "quota"]
		}]
	}`
	reg, err := ValidateRegistry([]byte(good))
	if err != nil {
		t.Fatalf("ValidateRegistry: %v", err)
	}
	if reg.Plugins[0].ID != "reset-priority" {
		t.Errorf("id = %s", reg.Plugins[0].ID)
	}

	bad := map[string]string{
		"missing author":  `{"schema_version":1,"plugins":[{"id":"x","name":"X","description":"d","repository":"https://github.com/a/b"}]}`,
		"bad repo":        `{"schema_version":1,"plugins":[{"id":"x","name":"X","description":"d","author":"a","repository":"https://gitlab.com/a/b"}]}`,
		"repo .git":       `{"schema_version":1,"plugins":[{"id":"x","name":"X","description":"d","author":"a","repository":"https://github.com/a/b.git"}]}`,
		"repo extra path": `{"schema_version":1,"plugins":[{"id":"x","name":"X","description":"d","author":"a","repository":"https://github.com/a/b/c"}]}`,
		"leading v":       `{"schema_version":1,"plugins":[{"id":"x","name":"X","description":"d","author":"a","version":"v1.0.0","repository":"https://github.com/a/b"}]}`,
		"bad schema":      `{"schema_version":7,"plugins":[]}`,
		"schema 0":        `{"schema_version":0,"plugins":[{"id":"x","name":"X","description":"d","author":"a","repository":"https://github.com/a/b"}]}`,
		"schema 2":        `{"schema_version":2,"plugins":[{"id":"x","name":"X","description":"d","author":"a","repository":"https://github.com/a/b"}]}`,
		"missing schema":  `{"plugins":[{"id":"x","name":"X","description":"d","author":"a","repository":"https://github.com/a/b"}]}`,
		"no plugins":      `{"schema_version":1,"plugins":[]}`,
		"not json":        `nope`,
	}
	for name, raw := range bad {
		if _, err := ValidateRegistry([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFormatChecksumsRoundTrip(t *testing.T) {
	in := map[string]string{
		"reset-priority_0.1.0_linux_amd64.zip":  strings.Repeat("ab", 32),
		"reset-priority_0.1.0_darwin_arm64.zip": strings.Repeat("CD", 32),
	}
	text, err := FormatChecksums(in)
	if err != nil {
		t.Fatalf("FormatChecksums: %v", err)
	}
	out, err := ParseChecksums(text)
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	if len(out) != 2 || out["reset-priority_0.1.0_darwin_arm64.zip"] != strings.Repeat("cd", 32) {
		t.Errorf("round trip = %v", out)
	}
	// Deterministic ordering: darwin sorts before linux.
	if !strings.HasSuffix(strings.SplitN(text, "\n", 2)[0], "darwin_arm64.zip") {
		t.Errorf("checksums not deterministically ordered:\n%s", text)
	}

	if _, err := FormatChecksums(map[string]string{"dist/x.zip": strings.Repeat("ab", 32)}); err == nil {
		t.Errorf("path-prefixed name accepted")
	}
	if _, err := FormatChecksums(map[string]string{"x.zip": "short"}); err == nil {
		t.Errorf("invalid sha accepted")
	}
}

func buildZip(t *testing.T, files map[string]string) *zip.Reader {
	t.Helper()
	entries := make([][2]string, 0, len(files))
	for name, content := range files {
		entries = append(entries, [2]string{name, content})
	}
	return buildZipEntries(t, entries)
}

func buildZipEntries(t *testing.T, entries [][2]string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, entry := range entries {
		f, err := w.Create(entry[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(entry[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestValidateArchiveLayout(t *testing.T) {
	id := plugin.PluginID

	good := buildZip(t, map[string]string{
		"reset-priority.so": "elf",
		"LICENSE":           "MIT",
	})
	if err := ValidateArchive(good, id, "linux"); err != nil {
		t.Errorf("valid archive rejected: %v", err)
	}

	cases := map[string]map[string]string{
		"missing license": {
			"reset-priority.so": "elf",
		},
		"missing library": {
			"LICENSE": "MIT",
		},
		"versioned library": {
			"reset-priority-v0.1.0.so": "elf",
			"LICENSE":                  "MIT",
		},
		"nested library": {
			"lib/reset-priority.so": "elf",
			"LICENSE":               "MIT",
		},
		"wrong plugin library": {
			"other-plugin.so": "elf",
			"LICENSE":         "MIT",
		},
		"foreign library": {
			"reset-priority.dll": "pe",
			"LICENSE":            "MIT",
		},
		"mixed native libraries": {
			"reset-priority.so":  "elf",
			"reset-priority.dll": "pe",
			"LICENSE":            "MIT",
		},
		"extra root file": {
			"reset-priority.so": "elf",
			"LICENSE":           "MIT",
			"README.md":         "hi",
		},
		"traversal": {
			"../reset-priority.so": "elf",
			"LICENSE":              "MIT",
		},
	}
	for name, files := range cases {
		if err := ValidateArchive(buildZip(t, files), id, "linux"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	duplicate := buildZipEntries(t, [][2]string{
		{"reset-priority.so", "elf-a"},
		{"reset-priority.so", "elf-b"},
		{"LICENSE", "MIT"},
	})
	if err := ValidateArchive(duplicate, id, "linux"); err == nil {
		t.Error("duplicate expected library accepted")
	}

	dll := buildZip(t, map[string]string{
		"reset-priority.dll": "pe",
		"LICENSE":            "MIT",
	})
	if err := ValidateArchive(dll, id, "windows"); err != nil {
		t.Errorf("windows archive rejected: %v", err)
	}
}

// TestRepositoryRegistryJSON validates the repo root registry.json. The file
// is a shipped release deliverable, so its absence is a hard failure rather
// than a skip.
func TestRepositoryRegistryJSON(t *testing.T) {
	path := filepath.Join("..", "..", "registry.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("registry.json must exist at the repository root: %v", err)
	}
	reg, err := ValidateRegistry(raw)
	if err != nil {
		t.Fatalf("registry.json invalid: %v", err)
	}
	found := false
	for _, p := range reg.Plugins {
		if p.ID == plugin.PluginID {
			found = true
		}
	}
	if !found {
		t.Errorf("registry.json does not declare plugin %q", plugin.PluginID)
	}
}
