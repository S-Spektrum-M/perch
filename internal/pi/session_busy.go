package pi

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"pircd/internal/engine"
)

// sessionProcess is advisory, not a lock: pi does not keep its session file
// open between writes. Explicit --session arguments cover that common case,
// but sessions selected interactively or via --resume can go undetected.
// Unreadable processes are skipped (including on systems without /proc).
func sessionProcess(file string) int {
	return sessionProcessIn("/proc", file, os.Getpid())
}

func sessionProcessIn(root, file string, self int) int {
	target, err := os.Stat(file)
	if err != nil {
		return 0
	}
	procs, _ := os.ReadDir(root)
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil || pid == self || !proc.IsDir() {
			continue
		}
		dir := filepath.Join(root, proc.Name())
		same := func(path string) bool {
			info, err := os.Stat(path)
			return err == nil && os.SameFile(target, info)
		}
		fds, _ := os.ReadDir(filepath.Join(dir, "fd"))
		for _, fd := range fds {
			if same(filepath.Join(dir, "fd", fd.Name())) {
				return pid
			}
		}
		cmd, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		args := strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00")
		for i, arg := range args {
			var path string
			if arg == "--session" && i+1 < len(args) {
				path = args[i+1]
			}
			if strings.HasPrefix(arg, "--session=") {
				path = strings.TrimPrefix(arg, "--session=")
			}
			if path == "" {
				continue
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, "cwd", path)
			}
			if same(path) {
				return pid
			}
		}
	}
	return 0
}

func CheckSessionAvailable(file string) error {
	if pid := sessionProcess(file); pid != 0 {
		return fmt.Errorf("%w (PID %d); close it there before attaching here", engine.ErrSessionBusy, pid)
	}
	return nil
}
