// Package bringup is the CLI's justfile-equivalent duty (cli.mdx §3): find the install, build it if
// needed, start the server detached with exactly the environment `just run` uses, refuse a
// foreign process on the port, and wait for /healthz.json.
package bringup

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/client"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/logger"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/progress"
)

// HealthWait bounds how long bring-up waits for the server (cli.mdx §3.1)
const HealthWait = 120 * time.Second

// Port returns the local port (EZBK_PORT, then the justfile's EBK_PORT, then 8080)
func Port() int {
	for _, name := range []string{"EZBK_PORT", "EBK_PORT"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
				return n
			}
		}
	}

	return 8080
}

// LocalURL is the default target
func LocalURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(Port())
}

// RepoRoot finds the ezBookkeeping checkout: EZBK_REPO, then the executable's ../.. (cli/bin/ezbk),
// then the working directory upward
func RepoRoot() (string, error) {
	isRoot := func(dir string) bool {
		_, e1 := os.Stat(filepath.Join(dir, "ezbookkeeping.go"))
		_, e2 := os.Stat(filepath.Join(dir, "conf", "ezbookkeeping.ini"))

		return e1 == nil && e2 == nil
	}

	if d := strings.TrimSpace(os.Getenv("EZBK_REPO")); d != "" {
		if isRoot(d) {
			return d, nil
		}

		return "", fmt.Errorf("EZBK_REPO=%s is not an ezBookkeeping checkout", d)
	}

	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}

		dir := filepath.Dir(exe)

		for i := 0; i < 4; i++ {
			if isRoot(dir) {
				return dir, nil
			}

			dir = filepath.Dir(dir)
		}
	}

	if wd, err := os.Getwd(); err == nil {
		dir := wd

		for i := 0; i < 8; i++ {
			if isRoot(dir) {
				return dir, nil
			}

			parent := filepath.Dir(dir)

			if parent == dir {
				break
			}

			dir = parent
		}
	}

	return "", errors.New("cannot find the ezBookkeeping checkout; set EZBK_REPO to its directory")
}

// PidFile and ServerLog live in the state dir
func PidFile() string   { return filepath.Join(logger.StateDir(), "server.pid") }
func ServerLog() string { return filepath.Join(logger.StateDir(), "server.log") }

// ReadPid returns the recorded pid of our server, if it is alive
func ReadPid() (int, bool) {
	data, err := os.ReadFile(PidFile())

	if err != nil {
		errfile.Expected("reading the optional server pid file", err)
		return 0, false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))

	if err != nil || pid <= 0 {
		errfile.Expected("parsing the server pid file", err)
		return 0, false
	}

	if err := syscall.Kill(pid, 0); err != nil {
		errfile.Expected("probing the recorded server pid", err)
		return pid, false
	}

	return pid, true
}

// PortHolder returns the pid and command holding a TCP port, via lsof (best effort)
func PortHolder(port int) (int, string) {
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpc").Output()

	if err != nil {
		errfile.Expected("probing the port holder with lsof", err)
		return 0, ""
	}

	pid, cmd := 0, ""
	sc := bufio.NewScanner(strings.NewReader(string(out)))

	for sc.Scan() {
		line := sc.Text()

		if strings.HasPrefix(line, "p") && pid == 0 {
			pid, _ = strconv.Atoi(line[1:])
		} else if strings.HasPrefix(line, "c") && cmd == "" {
			cmd = line[1:]
		}
	}

	return pid, cmd
}

// PortOpen reports whether something accepts connections on the port
func PortOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond)

	if err != nil {
		errfile.Expected("probing whether the port is open", err)
		return false
	}

	_ = conn.Close()

	return true
}

// Healthy probes /healthz.json
func Healthy(baseURL string) bool {
	m, err := client.Healthz(baseURL, 2*time.Second)

	return err == nil && fmt.Sprint(m["status"]) == "ok"
}

// Options for Up
type Options struct {
	AllowWrite bool
	AllowAdmin bool
	Quiet      bool
}

// Error is a bring-up failure carrying the log tail
type Error struct {
	Msg     string
	LogTail string
	Hint    string
}

func (e *Error) Error() string { return e.Msg }

// needsBuild reports whether the binary or the UI bundle is missing
func needsBuild(root string) bool {
	if _, err := os.Stat(filepath.Join(root, "ezbookkeeping")); err != nil {
		errfile.Expected("probing for the server binary", err)
		return true
	}

	if _, err := os.Stat(filepath.Join(root, "dist", "index.html")); err != nil {
		errfile.Expected("probing for the built UI bundle", err)
		return true
	}

	return false
}

// Up brings the server up if it is not, and waits for health. Already-up is a success.
func Up(opts Options) error {
	base := LocalURL()
	port := Port()

	if Healthy(base) {
		return nil
	}

	if PortOpen(port) {
		pid, cmd := PortHolder(port)
		ours, alive := ReadPid()

		if !(alive && ours == pid) {
			return &Error{
				Msg:  fmt.Sprintf("port %d is held by another process (pid %d %s) that is not an ezBookkeeping server", port, pid, cmd),
				Hint: fmt.Sprintf("stop that process yourself, or use another port: EZBK_PORT=%d ezbk up", port+10),
			}
		}
	}

	root, err := RepoRoot()

	if err != nil {
		return &Error{Msg: err.Error(), Hint: "export EZBK_REPO=/path/to/ezbookkeeping"}
	}

	sp := progress.New(opts.Quiet)

	if needsBuild(root) {
		sp.Start("Building ezBookkeeping (just build)…")

		cmd := exec.Command("just", "build")

		if _, err := exec.LookPath("just"); err != nil {
			errfile.Caught("looking up the just binary for the build", err)
			sp.Stop()
			return &Error{Msg: "the server is not built and `just` is not installed", Hint: "cd " + root + " && just build"}
		}

		cmd.Dir = root
		logf, _ := openServerLog()

		if logf != nil {
			cmd.Stdout, cmd.Stderr = logf, logf
		}

		err := cmd.Run()

		if logf != nil {
			_ = logf.Close()
		}

		sp.Stop()

		if err != nil {
			errfile.Caught("running just build", err)
			return &Error{Msg: "just build failed", LogTail: tail(ServerLog(), 30), Hint: "cd " + root + " && just build"}
		}
	}

	// The runtime state lives OUTSIDE the repo (CLAUDE.md "Runtime state location"): once real
	// statements are imported the database, the log and the uploads are private financial data,
	// and this repo is public. One-time migration: a database still in the repo's data/ moves.
	rt, err := RuntimeDirs(root)

	if err != nil {
		return &Error{Msg: err.Error(), Hint: "check ~/T/_ezbookkeeping is writable (EZBK_STATE_DIR overrides it)"}
	}

	secretPath := filepath.Join(rt.Data, ".secret_key")

	if info, err := os.Stat(secretPath); err != nil || info.Size() == 0 {
		errfile.Expected("probing for the existing data/.secret_key", err)
		// upstream's [security] secret_key (NOT the API secret key, apis.mdx §5.2)
		out, err := exec.Command("openssl", "rand", "-hex", "24").Output()

		if err != nil {
			errfile.Caught("generating data/.secret_key with openssl", err)
			return &Error{Msg: "cannot generate data/.secret_key (openssl missing?)", Hint: "cd " + root + " && just run once"}
		}

		if err := os.WriteFile(secretPath, []byte(strings.TrimSpace(string(out))), 0o600); err != nil {
			return &Error{Msg: "cannot write data/.secret_key: " + err.Error()}
		}
	}

	logf, err := openServerLog()

	if err != nil {
		return &Error{Msg: "cannot open " + ServerLog() + ": " + err.Error()}
	}

	cmd := exec.Command(filepath.Join(root, "ezbookkeeping"), "--conf-path", "conf/ezbookkeeping.ini", "server", "run")
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		"EBK_WORK_DIR="+root,
		"EBK_SERVER_HTTP_ADDR=127.0.0.1",
		"EBK_SERVER_HTTP_PORT="+strconv.Itoa(port),
		"EBK_SERVER_DOMAIN=localhost",
		"EBK_SERVER_STATIC_ROOT_PATH=dist",
		"EBKCFP_SECURITY_SECRET_KEY="+secretPath,
		"EBK_DATABASE_DB_PATH="+filepath.Join(rt.Data, "ezbookkeeping.db"),
		"EBK_LOG_LOG_PATH="+filepath.Join(rt.Log, "ezbookkeeping.log"),
		"EBK_STORAGE_LOCAL_FILESYSTEM_PATH="+rt.Storage+string(filepath.Separator),
	)

	cmd.Env = setEnv(cmd.Env, "EZBK_MACHINE_ALLOW_WRITE", boolEnv(opts.AllowWrite))
	cmd.Env = setEnv(cmd.Env, "EZBK_MACHINE_ALLOW_ADMIN", boolEnv(opts.AllowAdmin))

	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return &Error{Msg: "cannot start the server: " + err.Error()}
	}

	_ = os.WriteFile(PidFile(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	_ = cmd.Process.Release()
	_ = logf.Close()
	logger.Info("bring-up started server pid=%d port=%d write=%t admin=%t", cmd.Process.Pid, port, opts.AllowWrite, opts.AllowAdmin)

	sp.Start("Waiting for ezBookkeeping on " + base + "…")
	defer sp.Stop()

	deadline := time.Now().Add(HealthWait)

	for time.Now().Before(deadline) {
		if Healthy(base) {
			return nil
		}

		if _, alive := ReadPid(); !alive {
			sp.Stop()
			return &Error{Msg: "the server exited during start-up", LogTail: tail(ServerLog(), 30), Hint: "read " + ServerLog()}
		}

		time.Sleep(500 * time.Millisecond)
	}

	sp.Stop()

	return &Error{Msg: fmt.Sprintf("the server did not become healthy within %s", HealthWait), LogTail: tail(ServerLog(), 30), Hint: "read " + ServerLog()}
}

func boolEnv(b bool) string {
	if b {
		return "1"
	}

	return "0"
}

func setEnv(env []string, key, value string) []string {
	out := env[:0]

	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}

	return append(out, key+"="+value)
}

func openServerLog() (*os.File, error) {
	if err := os.MkdirAll(logger.StateDir(), 0o700); err != nil {
		return nil, err
	}

	return os.OpenFile(ServerLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// Stop stops OUR server (the recorded pid and its process group), never a foreign process
func Stop() (int, error) {
	pid, alive := ReadPid()

	if !alive {
		_ = os.Remove(PidFile())
		return 0, nil
	}

	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = syscall.Kill(pid, syscall.SIGTERM)

	for i := 0; i < 50; i++ {
		if err := syscall.Kill(pid, 0); err != nil {
			errfile.Expected("probing whether the stopped server has exited", err)
			_ = os.Remove(PidFile())
			return pid, nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(PidFile())

	return pid, nil
}

// Tail returns the last n lines of a file
func Tail(path string, n int) string { return tail(path, n) }

func tail(path string, n int) string {
	data, err := os.ReadFile(path)

	if err != nil {
		errfile.Expected("reading the log file to tail", err)
		return ""
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, "\n")
}

// Runtime is where the server keeps its state: {StateDir}/data (the sqlite database and upstream's
// secret_key), {StateDir}/log and {StateDir}/storage — never inside the repo
type Runtime struct{ Data, Log, Storage string }

// RuntimeDirs creates the runtime directories and moves a database (and its secret_key) left in
// the repo's data/ by an earlier version, so no user or book is lost in the move
func RuntimeDirs(root string) (*Runtime, error) {
	base := logger.StateDir()
	rt := &Runtime{Data: filepath.Join(base, "data"), Log: filepath.Join(base, "log"), Storage: filepath.Join(base, "storage")}

	for _, d := range []string{rt.Data, rt.Log, rt.Storage} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}

	for _, name := range []string{"ezbookkeeping.db", ".secret_key"} {
		old, dst := filepath.Join(root, "data", name), filepath.Join(rt.Data, name)

		if _, err := os.Stat(dst); err == nil {
			continue
		}

		if _, err := os.Stat(old); err != nil {
			errfile.Expected("looking for runtime state left in the repo", err)
			continue
		}

		if err := os.Rename(old, dst); err != nil {
			return nil, err
		}

		logger.Info("bring-up moved %s out of the repo to %s", name, dst)
	}

	return rt, nil
}
