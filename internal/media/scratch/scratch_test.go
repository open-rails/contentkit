package scratch

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// childEnv makes the test binary a worker-like process that opens its
// scratch under the named root, writes a file there and waits to be killed.
const childEnv = "CONTENTKIT_SCRATCH_CHILD"

func TestMain(m *testing.M) {
	if root := os.Getenv(childEnv); root != "" {
		d, err := Open(root)
		if err == nil {
			err = os.WriteFile(filepath.Join(d.Path(), "job"), []byte("in progress"), 0o600)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(d.Path())
		select {}
	}
	os.Exit(m.Run())
}

type process struct {
	cmd *exec.Cmd
	dir string
}

func start(t *testing.T, root string) *process {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childEnv+"="+root)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &process{cmd: cmd}
	t.Cleanup(func() { p.kill(t) })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	p.dir = strings.TrimSpace(line)
	return p
}

func (p *process) kill(t *testing.T) {
	t.Helper()
	if p.cmd.ProcessState != nil {
		return
	}
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = p.cmd.Wait()
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// A start removes what killed processes left and keeps what live ones,
// in other processes or its own, are using.
func TestOpenReclaimsOnlyDeadProcesses(t *testing.T) {
	root := t.TempDir()
	dead, live := start(t, root), start(t, root)
	dead.kill(t)
	if !exists(t, filepath.Join(dead.dir, "job")) {
		t.Fatal("a killed process's scratch vanished before any start")
	}
	d, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if exists(t, dead.dir) || exists(t, dead.dir+".lock") {
		t.Fatalf("a start kept a killed process's scratch: %s", dead.dir)
	}
	if !exists(t, filepath.Join(live.dir, "job")) {
		t.Fatalf("a start removed a live process's scratch: %s", live.dir)
	}
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(t, d.Path()) || second.Path() == d.Path() {
		t.Fatal("a second worker in one process took the first's scratch")
	}
	for _, own := range []*Dir{second, d} {
		if err := own.Close(); err != nil {
			t.Fatal(err)
		}
		if exists(t, own.Path()) || exists(t, own.Path()+".lock") {
			t.Fatalf("Close left %s", own.Path())
		}
	}
	live.kill(t)
	last, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer last.Close()
	if exists(t, live.dir) {
		t.Fatalf("a start kept a killed process's scratch: %s", live.dir)
	}
}

// A directory without a lock (a crash between Close's two removals) is
// nobody's.
func TestOpenRemovesUnlockedDirectories(t *testing.T) {
	root := t.TempDir()
	orphan := filepath.Join(root, base, "orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if exists(t, orphan) {
		t.Fatal("a start kept a directory no process locks")
	}
}
