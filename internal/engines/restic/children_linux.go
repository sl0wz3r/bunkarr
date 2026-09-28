package restic

import (
	"os"
	"strconv"
	"strings"
)

// resticChildren lists the PIDs of this process's children whose command name is restic
// (/proc/<pid>/stat: "pid (comm) state ppid …").
func resticChildren() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		open, closeIdx := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || closeIdx < open {
			continue
		}
		f := strings.Fields(s[closeIdx+1:])
		if len(f) < 2 || s[open+1:closeIdx] != "restic" {
			continue
		}
		if ppid, err := strconv.Atoi(f[1]); err == nil && ppid == self {
			out = append(out, pid)
		}
	}
	return out
}
