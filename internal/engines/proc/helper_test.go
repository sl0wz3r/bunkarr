package proc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The exec tests run this test binary as the "restic" and "rclone" programs. A child finds what
// to do in helperFile inside its HOME (the command's data directory), because the runner passes
// it no other environment (S22).
const helperFile = "proc-helper.json"

// helperChildArg marks the grandchild the "signals" mode starts.
const helperChildArg = "--proc-helper-child"

// helperConfig is a helper child's script.
type helperConfig struct {
	// Mode: env, cat, stat, signals, retry, sleep, stdin.
	Mode  string   `json:"mode"`
	Out   []string `json:"out,omitempty"`
	Err   []string `json:"err,omitempty"`
	Exit  int      `json:"exit,omitempty"`
	Log   string   `json:"log,omitempty"`
	Sleep string   `json:"sleep,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

func TestMain(m *testing.M) {
	if home := os.Getenv("HOME"); home != "" {
		if raw, err := os.ReadFile(filepath.Join(home, helperFile)); err == nil {
			var c helperConfig
			if err := json.Unmarshal(raw, &c); err != nil {
				fmt.Fprintln(os.Stderr, "helper config:", err)
				os.Exit(99)
			}
			os.Exit(helperMain(c))
		}
	}
	os.Exit(m.Run())
}

// helperMain is the child's behaviour.
func helperMain(c helperConfig) int {
	logf := func(format string, args ...any) {
		if c.Log == "" {
			return
		}
		f, err := os.OpenFile(c.Log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, format+"\n", args...)
	}
	switch c.Mode {
	case "env":
		env := os.Environ()
		slices.Sort(env)
		for _, kv := range env {
			fmt.Println(kv)
		}
		return 0
	case "cat":
		for _, l := range c.Out {
			fmt.Fprintln(os.Stdout, l)
		}
		for _, l := range c.Err {
			fmt.Fprintln(os.Stderr, l)
		}
		return c.Exit
	case "stat":
		for _, p := range c.Paths {
			fi, err := os.Lstat(p)
			if err != nil {
				fmt.Printf("%s missing\n", p)
				continue
			}
			raw, _ := os.ReadFile(p)
			fmt.Printf("%s %04o %s\n", p, fi.Mode().Perm(), raw)
		}
		return 0
	case "signals":
		role := "parent"
		if slices.Contains(os.Args, helperChildArg) {
			role = "child"
		}
		sigs := make(chan os.Signal, 8)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		if role == "parent" {
			self, _ := os.Executable()
			child := exec.Command(self, helperChildArg)
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if err := child.Start(); err != nil {
				fmt.Fprintln(os.Stderr, "start child:", err)
				return 98
			}
			// Wait until the child is up before saying ready.
			for i := 0; i < 500 && !logHas(c.Log, "child ready"); i++ {
				time.Sleep(10 * time.Millisecond)
			}
			fmt.Println("ready")
		} else {
			logf("child ready")
		}
		for s := range sigs {
			name := "INT"
			if s == syscall.SIGTERM {
				name = "TERM"
			}
			logf("%s %s", role, name)
		}
		return 0
	case "retry":
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT)
		for i := 0; ; i++ {
			select {
			case <-sigs:
				fmt.Fprintln(os.Stderr, "signal interrupt received, cleaning up")
				return 1
			case <-time.After(20 * time.Millisecond):
				fmt.Fprintf(os.Stderr, "Load(<config/0000000000>, 0, 0) returned error, retrying after %d ms: dial tcp: connection refused\n", i)
			}
		}
	case "sleep":
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT)
		d, _ := time.ParseDuration(c.Sleep)
		if d == 0 {
			d = time.Hour
		}
		select {
		case <-sigs:
			return 130
		case <-time.After(d):
			return c.Exit
		}
	case "stdin":
		_, _ = io.Copy(os.Stdout, os.Stdin)
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown helper mode", c.Mode)
	return 97
}

func logHas(path, text string) bool {
	raw, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(raw), text)
}

// testRunner is an exec runner whose restic and rclone are this test binary, with short stop
// delays.
func testRunner(t *testing.T, o ExecOptions) Runner {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	o.ResticPath, o.RclonePath = self, self
	if o.TermAfter == 0 {
		o.TermAfter = 300 * time.Millisecond
	}
	if o.KillAfter == 0 {
		o.KillAfter = 300 * time.Millisecond
	}
	if o.ReapDelay == 0 {
		o.ReapDelay = time.Second
	}
	return NewExecRunner(o)
}

// testRunDirs returns run directories in temp directories; tmpfs says whether statfs reports the
// shm directory as a tmpfs.
func testRunDirs(t *testing.T, tmpfs bool) (*RunDirs, string, string) {
	t.Helper()
	config, shm := t.TempDir(), t.TempDir()
	return NewRunDirs(config, RunDirOptions{ShmDir: shm, StatFS: func(string) (int64, error) {
		if tmpfs {
			return tmpfsMagic, nil
		}
		return 0x9123683e, nil // btrfs
	}}), config, shm
}

// helperDir creates a run directory whose child follows c.
func helperDir(t *testing.T, dirs *RunDirs, c helperConfig) *RunDir {
	t.Helper()
	rd, err := dirs.New(42)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	if err := rd.WriteData(helperFile, raw); err != nil {
		t.Fatal(err)
	}
	return rd
}

// collect reads every line of p, then waits.
func collect(t *testing.T, p Process) ([]Line, ExitStatus, error) {
	t.Helper()
	var lines []Line
	for l := range p.Lines() {
		lines = append(lines, l)
	}
	st, err := p.Wait()
	return lines, st, err
}

// linesUntil reads lines until one equals text, then keeps draining in the background.
func linesUntil(t *testing.T, p Process, text string) *sync.WaitGroup {
	t.Helper()
	var wg sync.WaitGroup
	found := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		once := sync.Once{}
		for l := range p.Lines() {
			if l.Text == text {
				once.Do(func() { close(found) })
			}
		}
		once.Do(func() { close(found) })
	}()
	select {
	case <-found:
	case <-time.After(10 * time.Second):
		t.Fatalf("no %q line", text)
	}
	return &wg
}

// readLines reads a file's lines.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// joinLines joins the texts of lines with newlines.
func joinLines(lines []Line) string {
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.Text
	}
	return strings.Join(texts, "\n")
}

// fmtAll formats v with every verb a log line or an error message might use.
func fmtAll(v any) string {
	return fmt.Sprintf("%v|%+v|%#v|%s", v, v, v, v)
}
