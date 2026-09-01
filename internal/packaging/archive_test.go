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

// writeFixtures creates a fake shared library and license in dir.
func writeFixtures(t *testing.T, dir string, libName string) (libPath, licensePath string) {
	t.Helper()
	libPath = filepath.Join(dir, libName)
	licensePath = filepath.Join(dir, "LICENSE")
	if err := os.WriteFile(libPath, []byte("fake shared library bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(licensePath, []byte("MIT License fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	return libPath, licensePath
}

func TestBuildReleaseArchiveLayoutAndSidecar(t *testing.T) {
	dir := t.TempDir()
	libPath, licensePath := writeFixtures(t, dir, "built.so")
	out := filepath.Join(dir, "dist")

	artifact, err := BuildReleaseArchive(out, plugin.PluginID, plugin.PluginVersion, "linux", "amd64", libPath, licensePath)
	if err != nil {
		t.Fatalf("BuildReleaseArchive: %v", err)
	}
	wantName := "reset-priority_" + plugin.PluginVersion + "_linux_amd64.zip"
	if artifact.ArchiveName != wantName {
		t.Errorf("archive name = %s, want %s", artifact.ArchiveName, wantName)
	}

	// The archive must satisfy the installer's layout rules and contain
	// exactly the library and LICENSE at the ZIP root.
	raw, err := os.ReadFile(artifact.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateArchive(reader, plugin.PluginID, "linux"); err != nil {
		t.Errorf("built archive rejected: %v", err)
	}
	names := map[string]bool{}
	for _, f := range reader.File {
		names[f.Name] = true
	}
	if !names["reset-priority.so"] || !names["LICENSE"] || len(names) != 2 {
		t.Errorf("archive entries = %v, want exactly reset-priority.so and LICENSE", names)
	}

	// Sidecar: bare filename, parseable, matching digest.
	sidecarRaw, err := os.ReadFile(artifact.ChecksumPath)
	if err != nil {
		t.Fatal(err)
	}
	sums, err := ParseChecksums(string(sidecarRaw))
	if err != nil {
		t.Fatalf("sidecar unparseable: %v", err)
	}
	if sums[artifact.ArchiveName] != artifact.SHA256 {
		t.Errorf("sidecar sum = %s, want %s", sums[artifact.ArchiveName], artifact.SHA256)
	}
}

func TestBuildReleaseArchiveDeterministic(t *testing.T) {
	dir := t.TempDir()
	libPath, licensePath := writeFixtures(t, dir, "built.dylib")

	first, err := BuildReleaseArchive(filepath.Join(dir, "a"), plugin.PluginID, plugin.PluginVersion, "darwin", "arm64", libPath, licensePath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildReleaseArchive(filepath.Join(dir, "b"), plugin.PluginID, plugin.PluginVersion, "darwin", "arm64", libPath, licensePath)
	if err != nil {
		t.Fatal(err)
	}
	rawA, _ := os.ReadFile(first.ArchivePath)
	rawB, _ := os.ReadFile(second.ArchivePath)
	if !bytes.Equal(rawA, rawB) {
		t.Errorf("identical inputs produced different archives")
	}
	if first.SHA256 != second.SHA256 {
		t.Errorf("digests differ: %s vs %s", first.SHA256, second.SHA256)
	}
}

func TestVerifyReleaseDirAndCombinedChecksums(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "dist")

	// Build the full five-platform release matrix from fixtures.
	for _, platform := range ReleasePlatforms {
		ext, err := LibraryExtension(platform.GOOS)
		if err != nil {
			t.Fatal(err)
		}
		libPath, licensePath := writeFixtures(t, t.TempDir(), "built"+ext)
		if _, err := BuildReleaseArchive(out, plugin.PluginID, plugin.PluginVersion, platform.GOOS, platform.GOARCH, libPath, licensePath); err != nil {
			t.Fatalf("BuildReleaseArchive(%s): %v", platform, err)
		}
	}

	sums, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, ReleasePlatforms)
	if err != nil {
		t.Fatalf("VerifyReleaseDir: %v", err)
	}
	if len(sums) != len(ReleasePlatforms) {
		t.Fatalf("verified %d archives, want %d", len(sums), len(ReleasePlatforms))
	}

	// Combined checksums.txt: bare filenames, deterministic, parseable.
	combined := filepath.Join(out, "checksums.txt")
	if err := WriteChecksumsFile(combined, sums); err != nil {
		t.Fatalf("WriteChecksumsFile: %v", err)
	}
	raw, err := os.ReadFile(combined)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseChecksums(string(raw))
	if err != nil {
		t.Fatalf("combined checksums unparseable: %v", err)
	}
	if len(parsed) != len(ReleasePlatforms) {
		t.Errorf("combined entries = %d, want %d", len(parsed), len(ReleasePlatforms))
	}
	for name, sum := range sums {
		if parsed[name] != sum {
			t.Errorf("%s: combined sum %s != %s", name, parsed[name], sum)
		}
	}
}

// buildPublishedRelease builds the full five-platform matrix, writes the
// combined checksums.txt, and removes the per-archive sidecars so the
// directory looks exactly like downloaded published GitHub release assets.
func buildPublishedRelease(t *testing.T) (out, checksumsPath string) {
	t.Helper()
	out = filepath.Join(t.TempDir(), "release")
	for _, platform := range ReleasePlatforms {
		ext, err := LibraryExtension(platform.GOOS)
		if err != nil {
			t.Fatal(err)
		}
		libPath, licensePath := writeFixtures(t, t.TempDir(), "built"+ext)
		if _, err := BuildReleaseArchive(out, plugin.PluginID, plugin.PluginVersion, platform.GOOS, platform.GOARCH, libPath, licensePath); err != nil {
			t.Fatalf("BuildReleaseArchive(%s): %v", platform, err)
		}
	}
	sums, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, ReleasePlatforms)
	if err != nil {
		t.Fatalf("VerifyReleaseDir: %v", err)
	}
	checksumsPath = filepath.Join(out, "checksums.txt")
	if err := WriteChecksumsFile(checksumsPath, sums); err != nil {
		t.Fatalf("WriteChecksumsFile: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sha256") {
			if err := os.Remove(filepath.Join(out, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	return out, checksumsPath
}

func TestVerifyPublishedReleaseDir(t *testing.T) {
	out, checksumsPath := buildPublishedRelease(t)

	sums, err := VerifyPublishedReleaseDir(out, plugin.PluginID, plugin.PluginVersion, ReleasePlatforms, checksumsPath)
	if err != nil {
		t.Fatalf("VerifyPublishedReleaseDir: %v", err)
	}
	if len(sums) != len(ReleasePlatforms) {
		t.Fatalf("verified %d archives, want %d", len(sums), len(ReleasePlatforms))
	}

	// Sidecar-mode verification must still fail on the same directory, since
	// published assets carry no per-archive sidecars.
	if _, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, ReleasePlatforms); err == nil {
		t.Error("sidecar-mode verification accepted a published-release directory without sidecars")
	}
}

func TestVerifyPublishedReleaseDirFailures(t *testing.T) {
	archive := "reset-priority_" + plugin.PluginVersion + "_linux_amd64.zip"

	t.Run("missing checksums file", func(t *testing.T) {
		out, checksumsPath := buildPublishedRelease(t)
		if err := os.Remove(checksumsPath); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyPublishedReleaseDir(out, plugin.PluginID, plugin.PluginVersion, ReleasePlatforms, checksumsPath); err == nil {
			t.Error("missing checksums.txt accepted")
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		out, checksumsPath := buildPublishedRelease(t)
		raw, err := os.ReadFile(checksumsPath)
		if err != nil {
			t.Fatal(err)
		}
		flipped := "0"
		if raw[0] == '0' {
			flipped = "1"
		}
		tampered := flipped + string(raw[1:])
		if err := os.WriteFile(checksumsPath, []byte(tampered), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyPublishedReleaseDir(out, plugin.PluginID, plugin.PluginVersion, ReleasePlatforms, checksumsPath); err == nil || !strings.Contains(err.Error(), "mismatch") {
			t.Errorf("tampered checksums.txt: err = %v, want mismatch", err)
		}
	})

	t.Run("entry for absent archive", func(t *testing.T) {
		out, checksumsPath := buildPublishedRelease(t)
		if err := os.Remove(filepath.Join(out, archive)); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyPublishedReleaseDir(out, plugin.PluginID, plugin.PluginVersion, nil, checksumsPath); err == nil {
			t.Error("checksums.txt entry without a matching archive accepted")
		}
	})

	t.Run("archive without entry", func(t *testing.T) {
		out, checksumsPath := buildPublishedRelease(t)
		raw, err := os.ReadFile(checksumsPath)
		if err != nil {
			t.Fatal(err)
		}
		sums, err := ParseChecksums(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		delete(sums, archive)
		if err := WriteChecksumsFile(checksumsPath, sums); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyPublishedReleaseDir(out, plugin.PluginID, plugin.PluginVersion, ReleasePlatforms, checksumsPath); err == nil || !strings.Contains(err.Error(), "no entry") {
			t.Errorf("archive missing from checksums.txt: err = %v, want no-entry error", err)
		}
	})

	t.Run("non-canonical checksums file", func(t *testing.T) {
		out, checksumsPath := buildPublishedRelease(t)
		raw, err := os.ReadFile(checksumsPath)
		if err != nil {
			t.Fatal(err)
		}
		upper := strings.ToUpper(string(raw[:64])) + string(raw[64:])
		if err := os.WriteFile(checksumsPath, []byte(upper), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyPublishedReleaseDir(out, plugin.PluginID, plugin.PluginVersion, ReleasePlatforms, checksumsPath); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Errorf("uppercase checksums.txt: err = %v, want canonical-format error", err)
		}
	})

	t.Run("missing required platform", func(t *testing.T) {
		out, checksumsPath := buildPublishedRelease(t)
		raw, err := os.ReadFile(checksumsPath)
		if err != nil {
			t.Fatal(err)
		}
		sums, err := ParseChecksums(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		delete(sums, archive)
		if err := WriteChecksumsFile(checksumsPath, sums); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(out, archive)); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyPublishedReleaseDir(out, plugin.PluginID, plugin.PluginVersion, ReleasePlatforms, checksumsPath); err == nil {
			t.Error("missing required platform accepted")
		}
	})
}

func TestVerifyReleaseDirFailures(t *testing.T) {
	base := t.TempDir()
	libPath, licensePath := writeFixtures(t, base, "built.so")

	build := func(t *testing.T) string {
		out := filepath.Join(t.TempDir(), "dist")
		if _, err := BuildReleaseArchive(out, plugin.PluginID, plugin.PluginVersion, "linux", "amd64", libPath, licensePath); err != nil {
			t.Fatal(err)
		}
		return out
	}
	linuxAmd64 := []Platform{{GOOS: "linux", GOARCH: "amd64"}}
	archive := "reset-priority_" + plugin.PluginVersion + "_linux_amd64.zip"

	t.Run("missing required platform", func(t *testing.T) {
		out := build(t)
		if _, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, ReleasePlatforms); err == nil {
			t.Error("missing platforms accepted")
		}
	})

	t.Run("tampered archive fails checksum", func(t *testing.T) {
		out := build(t)
		path := filepath.Join(out, archive)
		raw, _ := os.ReadFile(path)
		// Valid ZIP with valid layout but different bytes: rebuild with a
		// different library body, keeping the stale sidecar.
		if err := os.WriteFile(libPath, []byte("different bytes"), 0o755); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(t.TempDir(), "dist")
		if _, err := BuildReleaseArchive(other, plugin.PluginID, plugin.PluginVersion, "linux", "amd64", libPath, licensePath); err != nil {
			t.Fatal(err)
		}
		swapped, _ := os.ReadFile(filepath.Join(other, archive))
		if bytes.Equal(raw, swapped) {
			t.Fatal("fixture archives unexpectedly identical")
		}
		if err := os.WriteFile(path, swapped, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, linuxAmd64); err == nil || !strings.Contains(err.Error(), "mismatch") {
			t.Errorf("tampered archive: err = %v, want checksum mismatch", err)
		}
	})

	t.Run("missing sidecar", func(t *testing.T) {
		out := build(t)
		if err := os.Remove(filepath.Join(out, archive+".sha256")); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, linuxAmd64); err == nil {
			t.Error("missing sidecar accepted")
		}
	})

	t.Run("foreign zip name", func(t *testing.T) {
		out := build(t)
		if err := os.WriteFile(filepath.Join(out, "other-plugin_9.9.9_linux_amd64.zip"), []byte("zip?"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, linuxAmd64); err == nil {
			t.Error("foreign archive name accepted")
		}
	})

	t.Run("platform outside exact matrix", func(t *testing.T) {
		out := build(t)
		darwinLib, darwinLicense := writeFixtures(t, t.TempDir(), "built.dylib")
		if _, err := BuildReleaseArchive(out, plugin.PluginID, plugin.PluginVersion, "darwin", "arm64", darwinLib, darwinLicense); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, linuxAmd64); err == nil || !strings.Contains(err.Error(), "not in the required release matrix") {
			t.Errorf("extra platform archive: err = %v", err)
		}
	})

	t.Run("uppercase sidecar digest", func(t *testing.T) {
		out := build(t)
		path := filepath.Join(out, archive+".sha256")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) < 64 {
			t.Fatalf("short sidecar: %q", raw)
		}
		copy(raw[:64], strings.ToUpper(string(raw[:64])))
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, linuxAmd64); err == nil || !strings.Contains(err.Error(), "canonical lowercase") {
			t.Errorf("uppercase sidecar: err = %v", err)
		}
	})

	t.Run("sidecar with extra entry", func(t *testing.T) {
		out := build(t)
		path := filepath.Join(out, archive+".sha256")
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString(strings.Repeat("0", 64) + "  foreign.zip\n")
		closeErr := file.Close()
		if writeErr != nil {
			t.Fatal(writeErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if _, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, linuxAmd64); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("sidecar with extra entry: err = %v", err)
		}
	})

	t.Run("duplicate required platform", func(t *testing.T) {
		out := build(t)
		required := []Platform{{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "linux", GOARCH: "amd64"}}
		if _, err := VerifyReleaseDir(out, plugin.PluginID, plugin.PluginVersion, required); err == nil || !strings.Contains(err.Error(), "more than once") {
			t.Errorf("duplicate required platform: err = %v", err)
		}
	})

	t.Run("empty dir", func(t *testing.T) {
		if _, err := VerifyReleaseDir(t.TempDir(), plugin.PluginID, plugin.PluginVersion, nil); err == nil {
			t.Error("empty dir accepted")
		}
	})
}

func TestParsePlatforms(t *testing.T) {
	got, err := ParsePlatforms("linux_amd64, darwin_arm64")
	if err != nil {
		t.Fatalf("ParsePlatforms: %v", err)
	}
	if len(got) != 2 || got[0].String() != "linux_amd64" || got[1].String() != "darwin_arm64" {
		t.Errorf("ParsePlatforms = %v", got)
	}
	for _, bad := range []string{"", "linuxamd64", "linux_", "_amd64"} {
		if _, err := ParsePlatforms(bad); err == nil {
			t.Errorf("ParsePlatforms(%q) accepted", bad)
		}
	}
}
