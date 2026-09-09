// Package winfs provides a sandboxed filesystem for win-wings, replacing the
// openat2-based internal/ufs package from upstream wings.
//
// The sandbox is built on os.Root rather than hand-rolled path resolution.
// Nearly every security advisory in upstream wings' history has been a
// path-sandbox escape, so the guiding principle here is to minimise the amount
// of path resolution we own: os.Root is maintained by the Go team and receives
// security fixes we inherit for free.
//
// Windows path semantics are considerably more hostile than POSIX — junctions,
// alternate data streams, 8.3 short names, reserved device names, trailing dots
// and spaces, case insensitivity, and the \\?\ device namespace all provide
// ways to name the same file differently, or to name something that is not a
// file at all. os_root_probe_test.go documents which of these os.Root defends
// against and which remain our responsibility.
package winfs
