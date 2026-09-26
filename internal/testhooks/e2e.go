//go:build e2e

package testhooks

import (
	"bufio"
	"os"
)

// enabled is true in binaries built with -tags e2e.
const enabled = true

// maxFileValue bounds how much of a <name>_FILE file is read.
const maxFileValue = 4096

// lookup returns the environment variable name, or, when only <name>_FILE is set, the first line
// of that file (read on every call). A file that cannot be read is no override.
func lookup(name string) (string, bool) {
	if v, ok := os.LookupEnv(name); ok {
		return v, true
	}
	path, ok := os.LookupEnv(name + fileSuffix)
	if !ok || path == "" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, maxFileValue), maxFileValue)
	if !sc.Scan() {
		return "", false
	}
	return sc.Text(), true
}
