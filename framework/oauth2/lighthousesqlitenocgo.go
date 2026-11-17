//go:build !cgo

package oauth2

// The SQLite driver is unavailable in this build; never classify another
// backend's error as a SQLite lock or retry its write.
func isLighthouseSQLiteLock(error) bool { return false }
