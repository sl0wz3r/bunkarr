//go:build !e2e

package testhooks

// enabled is false in production builds: no override is ever read.
const enabled = false

// lookup never finds an override in a production build.
func lookup(string) (string, bool) { return "", false }
