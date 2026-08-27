package sandbox

import (
	"os"
	"reflect"
)

// mkdirAllForTest and symlinkForTest are thin wrappers the tests use instead of
// importing os directly at every call site. They exist so the symlink helper
// has one place to live: os.Symlink needs elevation or Developer Mode on
// Windows, and the caller decides whether that is a skip or a failure.
func mkdirAllForTest(path string) error { return os.MkdirAll(path, 0o755) }

// symlinkForTest creates a symbolic link, returning the raw OS error so a
// caller on a host without symlink permission can skip rather than fail.
func symlinkForTest(target, link string) error { return os.Symlink(target, link) }

// configFieldNames returns the field names of Config by reflection.
//
// It exists so a test can assert that a per-backend disclosure covers every
// setting Config can express, and have that assertion FAIL when a field is
// added rather than when someone next re-reads the file. Writing the field list
// down in the test instead would be a copy of the struct, and a copy stops
// describing the original at exactly the moment it matters.
func configFieldNames() []string {
	t := reflect.TypeOf(Config{})
	names := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		names = append(names, t.Field(i).Name)
	}
	return names
}
