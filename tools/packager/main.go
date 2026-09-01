// Command packager builds and verifies CPA Plugin Store release assets for
// reset-priority. It is invoked by the Makefile and the GitHub release
// workflow and works identically on Linux, macOS, and Windows runners.
//
// Build mode (default): package one natively built shared library into a
// deterministic <id>_<version>_<goos>_<goarch>.zip (library + LICENSE at the
// ZIP root) plus a bare-filename .sha256 sidecar:
//
//	go run ./tools/packager -lib reset-priority.so -version 0.1.0 -goos linux -goarch amd64 -out dist
//
// Verify mode: validate every release ZIP in -out against the store layout
// rules and its sidecar, enforce required platforms, and optionally write the
// combined bare-filename checksums.txt:
//
//	go run ./tools/packager -verify -version 0.1.0 -out dist \
//	    -require linux_amd64,linux_arm64,darwin_amd64,darwin_arm64,windows_amd64 \
//	    -combine dist/checksums.txt
//
// Published-release verify mode: downloaded GitHub release assets are the
// five ZIPs plus checksums.txt with no per-ZIP .sha256 sidecars, so pass
// -checksums to verify archive digests against the combined file instead of
// sidecars:
//
//	go run ./tools/packager -verify -version 0.1.0 -out dist/release-v0.1.0 \
//	    -require linux_amd64,linux_arm64,darwin_amd64,darwin_arm64,windows_amd64 \
//	    -checksums dist/release-v0.1.0/checksums.txt
//
// The tool refuses versions that do not match the compiled-in plugin version
// so a mistagged release cannot ship mismatched metadata.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/packaging"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/plugin"
)

func main() {
	var (
		verify    = flag.Bool("verify", false, "verify archives in -out instead of building one")
		lib       = flag.String("lib", "", "build: path to the natively built shared library")
		license   = flag.String("license", "LICENSE", "build: path to the LICENSE file included in the archive")
		version   = flag.String("version", plugin.PluginVersion, "release version without leading v; must match the compiled-in plugin version")
		goos      = flag.String("goos", runtime.GOOS, "build: target GOOS (native only; no cross-builds)")
		goarch    = flag.String("goarch", runtime.GOARCH, "build: target GOARCH (native only; no cross-builds)")
		out       = flag.String("out", "dist", "output/verify directory")
		require   = flag.String("require", "", "verify: comma-separated goos_goarch platforms that must all be present")
		combine   = flag.String("combine", "", "verify: write the combined bare-filename checksums.txt to this path")
		checksums = flag.String("checksums", "", "verify: verify downloaded published assets (no per-ZIP .sha256 sidecars) against this combined checksums.txt")
	)
	flag.Parse()

	if err := run(*verify, *lib, *license, *version, *goos, *goarch, *out, *require, *combine, *checksums); err != nil {
		fmt.Fprintln(os.Stderr, "packager:", err)
		os.Exit(1)
	}
}

func run(verify bool, lib, license, version, goos, goarch, out, require, combine, checksums string) error {
	if version != plugin.PluginVersion {
		return fmt.Errorf("version %q does not match compiled-in plugin version %q; bump PluginVersion in internal/plugin/runtime.go before tagging", version, plugin.PluginVersion)
	}
	if verify {
		return runVerify(version, out, require, combine, checksums)
	}
	if checksums != "" {
		return fmt.Errorf("-checksums is only valid with -verify")
	}
	return runBuild(lib, license, version, goos, goarch, out)
}

func runBuild(lib, license, version, goos, goarch, out string) error {
	if lib == "" {
		return fmt.Errorf("-lib is required in build mode")
	}
	artifact, err := packaging.BuildReleaseArchive(out, plugin.PluginID, version, goos, goarch, lib, license)
	if err != nil {
		return err
	}
	fmt.Printf("packaged %s\n", artifact.ArchivePath)
	fmt.Printf("  sha256  %s\n", artifact.SHA256)
	fmt.Printf("  sidecar %s\n", artifact.ChecksumPath)
	return nil
}

func runVerify(version, out, require, combine, checksums string) error {
	if checksums != "" && combine != "" {
		return fmt.Errorf("-checksums and -combine are mutually exclusive: published assets are verified against an existing checksums.txt, not used to write one")
	}
	var required []packaging.Platform
	if require != "" {
		parsed, err := packaging.ParsePlatforms(require)
		if err != nil {
			return err
		}
		required = parsed
	}
	var sums map[string]string
	var err error
	if checksums != "" {
		sums, err = packaging.VerifyPublishedReleaseDir(out, plugin.PluginID, version, required, checksums)
	} else {
		sums, err = packaging.VerifyReleaseDir(out, plugin.PluginID, version, required)
	}
	if err != nil {
		return err
	}
	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("verified %s  %s\n", sums[name], name)
	}
	if combine != "" {
		if err := packaging.WriteChecksumsFile(combine, sums); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d entries)\n", filepath.ToSlash(combine), len(sums))
	}
	fmt.Printf("release verification passed: %d archive(s)\n", len(sums))
	return nil
}
