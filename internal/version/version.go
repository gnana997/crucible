// Package version exposes the crucible build version. The value is set
// via -ldflags at build time and defaults to "dev" for unversioned builds.
package version

var version = "dev"

// String returns the current crucible version.
func String() string {
	return version
}
