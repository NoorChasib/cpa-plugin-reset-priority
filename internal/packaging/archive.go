package packaging

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// archiveEpoch is the fixed modification time written into every archive
// entry so that identical inputs produce byte-identical ZIPs. The ZIP format
// cannot represent instants before 1980.
var archiveEpoch = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// ArchiveFile is one root-level entry of a deterministic release archive.
type ArchiveFile struct {
	Name string
	Mode os.FileMode
	Body []byte
}

// WriteDeterministicZip writes entries sorted by name with a fixed timestamp
// and fixed modes, so the same inputs always yield byte-identical output.
// Every entry must be a bare root filename (the store installer requires the
// shared library at the ZIP root).
func WriteDeterministicZip(w io.Writer, files []ArchiveFile) error {
	if len(files) == 0 {
		return fmt.Errorf("archive needs at least one file")
	}
	sorted := append([]ArchiveFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	zw := zip.NewWriter(w)
	seen := make(map[string]bool, len(sorted))
	for _, f := range sorted {
		if f.Name == "" || strings.ContainsAny(f.Name, "/\\") {
			return fmt.Errorf("entry %q must be a bare root filename", f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate entry %q", f.Name)
		}
		seen[f.Name] = true
		header := &zip.FileHeader{
			Name:     f.Name,
			Method:   zip.Deflate,
			Modified: archiveEpoch,
		}
		header.SetMode(f.Mode)
		entry, err := zw.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("create %q: %w", f.Name, err)
		}
		if _, err := entry.Write(f.Body); err != nil {
			return fmt.Errorf("write %q: %w", f.Name, err)
		}
	}
	return zw.Close()
}

// ReleaseArtifact describes one built release asset pair.
type ReleaseArtifact struct {
	// ArchiveName is the bare asset name (<id>_<version>_<goos>_<goarch>.zip).
	ArchiveName string
	// ArchivePath is the path of the written ZIP.
	ArchivePath string
	// SHA256 is the lowercase hex digest of the ZIP.
	SHA256 string
	// ChecksumPath is the written "<ArchivePath>.sha256" sidecar containing
	// one "<sha256>  <bare-filename>" line.
	ChecksumPath string
}

// BuildReleaseArchive builds one deterministic store-compatible release ZIP
// (shared library plus LICENSE at the ZIP root), self-validates the layout,
// and writes the ZIP plus a bare-filename .sha256 sidecar into outDir.
func BuildReleaseArchive(outDir, id, version, goos, goarch, libPath, licensePath string) (ReleaseArtifact, error) {
	archiveName, err := ArchiveName(id, version, goos, goarch)
	if err != nil {
		return ReleaseArtifact{}, err
	}
	libName, err := LibraryBasename(id, goos)
	if err != nil {
		return ReleaseArtifact{}, err
	}
	libBody, err := os.ReadFile(libPath)
	if err != nil {
		return ReleaseArtifact{}, fmt.Errorf("read shared library: %w", err)
	}
	licenseBody, err := os.ReadFile(licensePath)
	if err != nil {
		return ReleaseArtifact{}, fmt.Errorf("read license: %w", err)
	}

	var buf bytes.Buffer
	if err := WriteDeterministicZip(&buf, []ArchiveFile{
		{Name: libName, Mode: 0o755, Body: libBody},
		{Name: "LICENSE", Mode: 0o644, Body: licenseBody},
	}); err != nil {
		return ReleaseArtifact{}, err
	}

	// Self-check the produced archive against the installer's layout rules
	// before anything is written to disk.
	reader, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return ReleaseArtifact{}, fmt.Errorf("reopen built archive: %w", err)
	}
	if err := ValidateArchive(reader, id, goos); err != nil {
		return ReleaseArtifact{}, fmt.Errorf("built archive failed layout validation: %w", err)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return ReleaseArtifact{}, err
	}
	archivePath := filepath.Join(outDir, archiveName)
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		return ReleaseArtifact{}, err
	}

	digest := sha256.Sum256(buf.Bytes())
	sum := hex.EncodeToString(digest[:])
	sidecar, err := FormatChecksums(map[string]string{archiveName: sum})
	if err != nil {
		return ReleaseArtifact{}, err
	}
	checksumPath := archivePath + ".sha256"
	if err := os.WriteFile(checksumPath, []byte(sidecar), 0o644); err != nil {
		return ReleaseArtifact{}, err
	}
	return ReleaseArtifact{
		ArchiveName:  archiveName,
		ArchivePath:  archivePath,
		SHA256:       sum,
		ChecksumPath: checksumPath,
	}, nil
}

// Platform is one GOOS/GOARCH release target.
type Platform struct {
	GOOS   string
	GOARCH string
}

// String renders the canonical goos_goarch form.
func (p Platform) String() string { return p.GOOS + "_" + p.GOARCH }

// ReleasePlatforms is the exact v0.1.0 release matrix. Linux uses matching-
// architecture runners and manylinux2014 containers; macOS and Windows use
// matching native runners. CGO c-shared builds are not cross-compiled, so
// windows/arm64 and freebsd are intentionally absent.
var ReleasePlatforms = []Platform{
	{GOOS: "linux", GOARCH: "amd64"},
	{GOOS: "linux", GOARCH: "arm64"},
	{GOOS: "darwin", GOARCH: "amd64"},
	{GOOS: "darwin", GOARCH: "arm64"},
	{GOOS: "windows", GOARCH: "amd64"},
}

// ParsePlatforms parses a comma-separated "goos_goarch,goos_goarch" list.
func ParsePlatforms(csv string) ([]Platform, error) {
	var out []Platform
	for _, item := range strings.Split(csv, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, "_")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("platform %q is not goos_goarch", item)
		}
		out = append(out, Platform{GOOS: parts[0], GOARCH: parts[1]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no platforms given")
	}
	return out, nil
}

// parseArchiveName recovers the platform from
// "<id>_<version>_<goos>_<goarch>.zip"; ok is false when the name does not
// belong to this id/version.
func parseArchiveName(name, id, version string) (Platform, bool) {
	prefix := id + "_" + version + "_"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".zip") {
		return Platform{}, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".zip")
	parts := strings.Split(rest, "_")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Platform{}, false
	}
	return Platform{GOOS: parts[0], GOARCH: parts[1]}, true
}

// VerifyReleaseDir validates every release ZIP in dir against the store
// layout rules and its canonical lowercase .sha256 sidecar. When required is
// non-empty, it is an exact platform allowlist: missing and additional
// platform archives are both rejected. The returned map is suitable for
// FormatChecksums.
func VerifyReleaseDir(dir, id, version string, required []Platform) (map[string]string, error) {
	return verifyReleaseDir(dir, id, version, required, true)
}

// VerifyPublishedReleaseDir validates downloaded published release assets.
// A published release carries only the release ZIPs plus one combined
// checksums.txt — no per-archive .sha256 sidecars — so every ZIP in dir is
// checked against the store layout rules and its digest is verified against
// the canonical combined checksums file at checksumsPath instead of a
// sidecar. The checksums file must list exactly the archives present in dir.
func VerifyPublishedReleaseDir(dir, id, version string, required []Platform, checksumsPath string) (map[string]string, error) {
	sums, err := verifyReleaseDir(dir, id, version, required, false)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(checksumsPath)
	if err != nil {
		return nil, fmt.Errorf("read combined checksums file: %w", err)
	}
	listed, err := ParseChecksums(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", checksumsPath, err)
	}
	canonical, err := FormatChecksums(listed)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", checksumsPath, err)
	}
	if string(raw) != canonical {
		return nil, fmt.Errorf("%s is not canonical lowercase bare-filename sha256sum format", checksumsPath)
	}
	for name := range listed {
		if _, ok := sums[name]; !ok {
			return nil, fmt.Errorf("%s lists %q, which is not a release archive in %s", checksumsPath, name, dir)
		}
	}
	for name, sum := range sums {
		want, ok := listed[name]
		if !ok {
			return nil, fmt.Errorf("%s has no entry for archive %q", checksumsPath, name)
		}
		if want != sum {
			return nil, fmt.Errorf("%s: sha256 mismatch: archive %s, checksums file %s", name, sum, want)
		}
	}
	return sums, nil
}

// verifyReleaseDir is the shared core of VerifyReleaseDir and
// VerifyPublishedReleaseDir. Per-archive .sha256 sidecars are required and
// verified only when requireSidecars is true; the local packager build always
// writes them, but published GitHub release assets do not include them.
func verifyReleaseDir(dir, id, version string, required []Platform, requireSidecars bool) (map[string]string, error) {
	allowed := make(map[string]bool, len(required))
	for _, platform := range required {
		key := platform.String()
		if allowed[key] {
			return nil, fmt.Errorf("required platform %s is listed more than once", platform)
		}
		allowed[key] = true
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sums := make(map[string]string)
	found := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".zip") {
			continue
		}
		platform, ok := parseArchiveName(name, id, version)
		if !ok {
			return nil, fmt.Errorf("unexpected archive %q: want %s_%s_<goos>_<goarch>.zip", name, id, version)
		}
		platformKey := platform.String()
		if len(allowed) > 0 && !allowed[platformKey] {
			return nil, fmt.Errorf("unexpected platform archive %q: %s is not in the required release matrix", name, platform)
		}
		if found[platformKey] {
			return nil, fmt.Errorf("multiple archives found for platform %s", platform)
		}
		raw, errRead := os.ReadFile(filepath.Join(dir, name))
		if errRead != nil {
			return nil, errRead
		}
		reader, errZip := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if errZip != nil {
			return nil, fmt.Errorf("%s: not a valid ZIP: %w", name, errZip)
		}
		if errLayout := ValidateArchive(reader, id, platform.GOOS); errLayout != nil {
			return nil, fmt.Errorf("%s: %w", name, errLayout)
		}
		digest := sha256.Sum256(raw)
		sum := hex.EncodeToString(digest[:])

		if requireSidecars {
			sidecarRaw, errSidecar := os.ReadFile(filepath.Join(dir, name+".sha256"))
			if errSidecar != nil {
				return nil, fmt.Errorf("%s: missing .sha256 sidecar: %w", name, errSidecar)
			}
			sidecar, errParse := ParseChecksums(string(sidecarRaw))
			if errParse != nil {
				return nil, fmt.Errorf("%s.sha256: %w", name, errParse)
			}
			if len(sidecar) != 1 {
				return nil, fmt.Errorf("%s.sha256 must contain exactly one checksum entry", name)
			}
			want, ok := sidecar[name]
			if !ok {
				return nil, fmt.Errorf("%s.sha256 has no entry for bare filename %q", name, name)
			}
			canonicalSidecar, errCanonical := FormatChecksums(map[string]string{name: want})
			if errCanonical != nil {
				return nil, fmt.Errorf("%s.sha256: %w", name, errCanonical)
			}
			if string(sidecarRaw) != canonicalSidecar {
				return nil, fmt.Errorf("%s.sha256 is not canonical lowercase bare-filename sha256sum format", name)
			}
			if want != sum {
				return nil, fmt.Errorf("%s: sha256 mismatch: archive %s, sidecar %s", name, sum, want)
			}
		}
		sums[name] = sum
		found[platformKey] = true
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("no %s_%s_*.zip archives found in %s", id, version, dir)
	}
	for _, req := range required {
		if !found[req.String()] {
			return nil, fmt.Errorf("required platform %s has no archive in %s", req, dir)
		}
	}
	return sums, nil
}

// WriteChecksumsFile writes the combined bare-filename checksums.txt and
// verifies it round-trips through the installer's parser.
func WriteChecksumsFile(path string, sums map[string]string) error {
	text, err := FormatChecksums(sums)
	if err != nil {
		return err
	}
	if _, err := ParseChecksums(text); err != nil {
		return fmt.Errorf("generated checksums file failed self-parse: %w", err)
	}
	return os.WriteFile(path, []byte(text), 0o644)
}
