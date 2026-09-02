// cpa-plugin-reset-priority is a CLIProxyAPI native plugin that maintains
// credential priorities for Claude and Codex OAuth accounts so that the
// account whose regular weekly quota resets soonest is consumed first.
//
// Build as a CPA-loadable shared library (the exported ABI entrypoint lives
// in abi.go and requires CGO):
//
//	CGO_ENABLED=1 go build -buildmode=c-shared -o reset-priority.so .
//
// The library basename defines the plugin ID: reset-priority.
package main

// The embedded tzdata copy lets display-timezone resolve IANA zone names
// even when the host container image ships without /usr/share/zoneinfo.
import _ "time/tzdata"

// main is required by buildmode=c-shared; the host never calls it.
func main() {}
