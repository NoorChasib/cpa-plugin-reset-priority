// Package packaging encodes the CPA Plugin Store artifact and registry
// naming rules so they can be validated in CI before release workflows
// exist. Rules audited against CLIProxyAPI commit
// 81e1b5374f99c212f196f34956eeed964a46b8fa and the official
// CLIProxyAPI-Plugins-Store registry (schema_version 1).
package packaging

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// pluginIDPattern is the host's plugin ID rule.
var pluginIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// sha256Pattern matches one lowercase/uppercase 64-hex checksum.
var sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// ValidatePluginID checks the host's plugin ID rule.
func ValidatePluginID(id string) error {
	if !pluginIDPattern.MatchString(id) {
		return fmt.Errorf("plugin id %q does not match %s", id, pluginIDPattern.String())
	}
	return nil
}

// LibraryExtension returns the shared-library extension for a GOOS.
func LibraryExtension(goos string) (string, error) {
	switch goos {
	case "linux", "freebsd":
		return ".so", nil
	case "darwin":
		return ".dylib", nil
	case "windows":
		return ".dll", nil
	default:
		return "", fmt.Errorf("unsupported GOOS %q", goos)
	}
}

// LibraryBasename returns the required shared-library name at ZIP root
// (unversioned form): <id>.<ext>.
func LibraryBasename(id, goos string) (string, error) {
	if err := ValidatePluginID(id); err != nil {
		return "", err
	}
	ext, err := LibraryExtension(goos)
	if err != nil {
		return "", err
	}
	return id + ext, nil
}

// ArchiveName returns the required release ZIP name for GitHub-release
// installs: <id>_<version>_<goos>_<goarch>.zip. Version must not carry a
// leading v.
func ArchiveName(id, version, goos, goarch string) (string, error) {
	if err := ValidatePluginID(id); err != nil {
		return "", err
	}
	if version == "" || strings.HasPrefix(version, "v") || strings.HasPrefix(version, "V") {
		return "", fmt.Errorf("version %q must be non-empty without a leading v", version)
	}
	if goos == "" || goarch == "" {
		return "", fmt.Errorf("goos and goarch are required")
	}
	return fmt.Sprintf("%s_%s_%s_%s.zip", id, version, goos, goarch), nil
}

// ParseChecksums parses checksums.txt lines of the form
// "<sha256>  <filename>". Filenames must be bare asset names: the host looks
// entries up by the bare GitHub asset name, so path prefixes (./x, dist/x)
// break installation.
func ParseChecksums(content string) (map[string]string, error) {
	out := make(map[string]string)
	for lineNo, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: expected \"<sha256> <filename>\"", lineNo+1)
		}
		sum, name := fields[0], fields[1]
		if !sha256Pattern.MatchString(sum) {
			return nil, fmt.Errorf("line %d: %q is not a sha256 hex digest", lineNo+1, sum)
		}
		if strings.ContainsAny(name, "/\\") {
			return nil, fmt.Errorf("line %d: filename %q must be a bare asset name without path separators", lineNo+1, name)
		}
		out[name] = strings.ToLower(sum)
	}
	return out, nil
}

// FormatChecksums renders a checksums.txt with deterministic ordering in the
// "<sha256>  <bare-filename>" format the host installer parses.
func FormatChecksums(sums map[string]string) (string, error) {
	names := make([]string, 0, len(sums))
	for name := range sums {
		if strings.ContainsAny(name, "/\\") {
			return "", fmt.Errorf("filename %q must be a bare asset name", name)
		}
		if !sha256Pattern.MatchString(sums[name]) {
			return "", fmt.Errorf("%q has invalid sha256 %q", name, sums[name])
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s  %s\n", strings.ToLower(sums[name]), name)
	}
	return b.String(), nil
}

// ValidateArchive enforces the exact release ZIP root allowlist: the
// unversioned target library and LICENSE, both regular files at the ZIP root.
// Any extra entry is rejected, including a foreign-platform native library,
// a versioned library, a nested path, or a directory.
func ValidateArchive(r *zip.Reader, id, goos string) error {
	libName, err := LibraryBasename(id, goos)
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		libName:   true,
		"LICENSE": true,
	}
	seen := make(map[string]bool, len(allowed))
	for _, f := range r.File {
		name := f.Name
		if name == "" || strings.ContainsAny(name, "/\\") {
			return fmt.Errorf("entry %q must be a bare ZIP-root filename", name)
		}
		if !f.Mode().IsRegular() {
			return fmt.Errorf("entry %q is not a regular file", name)
		}
		if !allowed[name] {
			return fmt.Errorf("unexpected archive entry %q: only %q and LICENSE are allowed", name, libName)
		}
		if seen[name] {
			return fmt.Errorf("duplicate archive entry %q", name)
		}
		seen[name] = true
	}
	for name := range allowed {
		if !seen[name] {
			return fmt.Errorf("required archive entry %q is missing", name)
		}
	}
	return nil
}

// Registry mirrors the store registry.json shape (schema 1).
type Registry struct {
	SchemaVersion int              `json:"schema_version"`
	Plugins       []RegistryPlugin `json:"plugins"`
}

// RegistryPlugin is one registry entry.
type RegistryPlugin struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Version     string   `json:"version,omitempty"`
	Repository  string   `json:"repository"`
	Homepage    string   `json:"homepage,omitempty"`
	License     string   `json:"license,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// githubRepoPattern is the exact repository shape required for
// github-release installs.
var githubRepoPattern = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+$`)

// ValidateRegistry checks required fields for a github-release registry.
func ValidateRegistry(raw []byte) (*Registry, error) {
	var reg Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("registry.json is not valid JSON: %w", err)
	}
	// The audited store registry contract is schema_version 1 exactly. Any
	// other version (including a future 2) has unaudited semantics and must be
	// rejected rather than assumed compatible.
	if reg.SchemaVersion != 1 {
		return nil, fmt.Errorf("schema_version must be exactly 1, got %d", reg.SchemaVersion)
	}
	if len(reg.Plugins) == 0 {
		return nil, fmt.Errorf("registry has no plugins")
	}
	for i, p := range reg.Plugins {
		if err := ValidatePluginID(p.ID); err != nil {
			return nil, fmt.Errorf("plugins[%d]: %w", i, err)
		}
		if p.Name == "" || p.Description == "" || p.Author == "" {
			return nil, fmt.Errorf("plugins[%d] (%s): name, description, and author are required", i, p.ID)
		}
		if !githubRepoPattern.MatchString(p.Repository) {
			return nil, fmt.Errorf("plugins[%d] (%s): repository must be exactly https://github.com/{owner}/{repo}", i, p.ID)
		}
		if strings.HasSuffix(p.Repository, ".git") {
			return nil, fmt.Errorf("plugins[%d] (%s): repository must not end in .git", i, p.ID)
		}
		if p.Version != "" && (strings.HasPrefix(p.Version, "v") || strings.HasPrefix(p.Version, "V")) {
			return nil, fmt.Errorf("plugins[%d] (%s): version must omit the leading v", i, p.ID)
		}
	}
	return &reg, nil
}
