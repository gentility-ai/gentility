//go:build !darwin

package tsnode

// HintDefaultRoute has nothing to tell on a platform whose library does not
// learn the default route from the app. It exists so callers build and test
// on Linux (CI); the phone is always darwin.
func HintDefaultRoute(string) {}
