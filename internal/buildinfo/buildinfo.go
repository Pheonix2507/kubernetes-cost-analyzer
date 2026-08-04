// Package buildinfo carries build-time metadata that the linker injects.
//
// WHY THIS PACKAGE EXISTS
// -----------------------
// When a service misbehaves in production, the first question is always "which
// version is actually running?" Answering it from the deployed image tag is
// unreliable: tags get overwritten, `latest` means nothing, and a rebuilt image can
// carry the same tag as different code. So the binary itself must be able to say
// which commit it was built from.
//
// WHY A SEPARATE PACKAGE RATHER THAN VARIABLES IN main
// ---------------------------------------------------
// The linker sets these with -X <full/import/path>.<VarName>=<value>. If they lived
// in package main, we would need a different -X flag per binary (cmd/api and
// cmd/collector have different import paths), and the Makefile would have to
// duplicate every flag. One shared package means one set of flags for every binary
// we ever add. See LDFLAGS in the Makefile.
package buildinfo

import (
	"fmt"
	"runtime"
)

// These are deliberately `var` and not `const`.
//
// The linker's -X flag can ONLY overwrite a string variable. Declaring these as
// constants compiles fine and silently ignores every -X flag, so every build would
// report "dev" forever with no error to tell you why. This is a genuinely common
// mistake and it fails silently, which is the worst way to fail.
//
// The defaults are what you get from a plain `go build` or `go run` with no flags,
// which is exactly right for local development.
var (
	// Version is a semver tag, or a git describe string, or "dev".
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "unknown"
	// BuildTime is an RFC3339 UTC timestamp of when the binary was linked.
	BuildTime = "unknown"
)

// String returns a single-line human-readable summary, for --version output and
// startup logs.
func String() string {
	return fmt.Sprintf("version=%s commit=%s built=%s go=%s %s/%s",
		Version, Commit, BuildTime, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// LogAttrs returns the fields we attach to every log line, so that any log we
// examine later can be traced back to the exact build that produced it.
//
// Returning []any rather than []slog.Attr keeps this package free of any logging
// dependency: it stays a plain data package that anything can consume.
func LogAttrs() []any {
	return []any{
		"version", Version,
		"commit", Commit,
	}
}
