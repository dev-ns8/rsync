package rsynctest

import (
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/gokrazy/rsync/internal/anonssh"
	"github.com/gokrazy/rsync/internal/config"
	"github.com/gokrazy/rsync/internal/maincmd"
	"github.com/gokrazy/rsync/internal/rsyncd"
	"golang.org/x/sys/unix"
)

type TestServer struct {
	listeners []config.Listener

	// Port is the port on which the test server is listening on. Useful to pass
	// to rsync’s --port option.
	Port string
}

// InteropModMap is a convenience function to define an rsync module named
// “interop” with the specified path.
func InteropModMap(path string) map[string]config.Module {
	return map[string]config.Module{
		"interop": {
			Name: "interop",
			Path: path,
		},
	}
}

type Option func(ts *TestServer)

func Listeners(lns []config.Listener) Option {
	return func(ts *TestServer) {
		ts.listeners = lns
	}
}

func New(t *testing.T, modMap map[string]config.Module, opts ...Option) *TestServer {
	ts := &TestServer{}
	for _, opt := range opts {
		opt(ts)
	}
	if len(ts.listeners) == 0 {
		ts.listeners = []config.Listener{
			{Rsyncd: "localhost:0"},
		}
	}
	srv := &rsyncd.Server{
		Modules: modMap,
	}

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	log.Printf("listening on %s", ln.Addr())
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ts.Port = port

	if ts.listeners[0].AnonSSH != "" {
		cfg := &config.Config{
			ModuleMap: modMap,
		}
		go func() {
			err := anonssh.Serve(ln, cfg, func(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
				return maincmd.Main(args, stdin, stdout, stderr, cfg)
			})

			if errors.Is(err, net.ErrClosed) {
				return
			}

			if err != nil {
				log.Print(err)
			}
		}()
	} else {
		go srv.Serve(ln)
	}

	return ts
}

var rsyncVersionRe = regexp.MustCompile(`rsync\s*version ([v0-9.]+)`)

func RsyncVersion(t *testing.T) string {
	version := exec.Command("rsync", "--version")
	version.Stderr = os.Stderr
	b, err := version.Output()
	if err != nil {
		t.Fatalf("%v: %v", version.Args, err)
	}
	matches := rsyncVersionRe.FindStringSubmatch(string(b))
	if len(matches) == 0 {
		t.Fatalf("rsync: version number not found in rsync --version output")
	}
	// rsync 2.6.9 does not print a v prefix,
	// but rsync v3.2.3 does print a v prefix.
	return strings.TrimPrefix(matches[1], "v")
}

func CreateDummyDeviceFiles(t *testing.T, dir string) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	char := filepath.Join(dir, "char")
	// major 1, minor 5, like /dev/zero
	if err := unix.Mknod(char, 0600|syscall.S_IFCHR, int(unix.Mkdev(1, 5))); err != nil {
		t.Fatal(err)
	}

	block := filepath.Join(dir, "block")
	// major 242, minor 9, like /dev/nvme0
	if err := unix.Mknod(block, 0600|syscall.S_IFBLK, int(unix.Mkdev(242, 9))); err != nil {
		t.Fatal(err)
	}

	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
}

func VerifyDummyDeviceFiles(t *testing.T, source, dest string) {
	{
		sourcest, err := os.Stat(filepath.Join(source, "char"))
		if err != nil {
			t.Fatal(err)
		}
		destst, err := os.Stat(filepath.Join(dest, "char"))
		if err != nil {
			t.Fatal(err)
		}
		if destst.Mode().Type()&os.ModeCharDevice == 0 {
			t.Fatalf("unexpected type: got %v, want character device", destst.Mode())
		}
		destsys, ok := destst.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("stat does not contain rdev")
		}
		sourcesys, ok := sourcest.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("stat does not contain rdev")
		}
		if got, want := destsys.Rdev, sourcesys.Rdev; got != want {
			t.Fatalf("unexpected rdev: got %v, want %v", got, want)
		}
	}

	{
		sourcest, err := os.Stat(filepath.Join(source, "block"))
		if err != nil {
			t.Fatal(err)
		}
		destst, err := os.Stat(filepath.Join(dest, "block"))
		if err != nil {
			t.Fatal(err)
		}
		if destst.Mode().Type()&os.ModeDevice == 0 ||
			destst.Mode().Type()&os.ModeCharDevice != 0 {
			t.Fatalf("unexpected type: got %v, want block device", destst.Mode())
		}
		destsys, ok := destst.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("stat does not contain rdev")
		}
		sourcesys, ok := sourcest.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("stat does not contain rdev")
		}
		if got, want := destsys.Rdev, sourcesys.Rdev; got != want {
			t.Fatalf("unexpected rdev: got %v, want %v", got, want)
		}
	}

	{
		st, err := os.Stat(filepath.Join(dest, "fifo"))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Type()&os.ModeNamedPipe == 0 {
			t.Fatalf("unexpected type: got %v, want fifo", st.Mode())
		}
	}

	{
		st, err := os.Stat(filepath.Join(dest, "sock"))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Type()&os.ModeSocket == 0 {
			t.Fatalf("unexpected type: got %v, want socket", st.Mode())
		}
	}

}
