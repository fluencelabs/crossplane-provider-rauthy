// Package version carries the provider version, set at build time.
package version

// Version is the provider version. The build overrides it with -ldflags.
//
//nolint:gochecknoglobals // Set by the linker, so it has to be a variable.
var Version = "dev"
