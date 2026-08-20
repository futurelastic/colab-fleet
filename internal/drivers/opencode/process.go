package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Availability is what a Probe-style check reports about the local
// opencode install, established WITHOUT starting a server and WITHOUT
// committing to any working directory — colab-fleet issue #55's third
// deliverable: "absent install is a first-class answer, not a startup
// crash."
type Availability struct {
	// Installed is true when the binary was found and answered --version.
	Installed bool
	// Path is the resolved executable, set whenever LookPath succeeded —
	// even if the version check that follows it then failed, so a caller
	// diagnosing a broken install still learns which binary was tried.
	Path string
	// Version is `opencode --version`'s trimmed output, best effort.
	Version string
	// Err explains why Installed is false. Nil when Installed is true.
	Err error
}

// Probe reports whether opencode can be found and run on this machine.
// bin overrides the default lookup ("opencode" on PATH); empty uses the
// default. It never starts a server and never touches a working
// directory — the two things #55 flagged as not this check's business.
func Probe(ctx context.Context, bin string) Availability {
	if bin == "" {
		bin = defaultBin
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return Availability{Err: fmt.Errorf("opencode: %q not found on PATH: %w", bin, err)}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--version").Output()
	if err != nil {
		return Availability{Path: path, Err: fmt.Errorf("opencode: %q --version failed: %w", path, err)}
	}
	return Availability{Installed: true, Path: path, Version: strings.TrimSpace(string(out))}
}

// process owns one spawned opencode server subprocess.
type process struct {
	cmd      *exec.Cmd
	baseURL  string
	password string
}

// freePort asks the OS for an unused TCP port on 127.0.0.1 and returns it
// immediately available for reuse.
//
// This is how "we choose the port" (#55) is actually true rather than
// aspirational: opencode's --port flag takes a number we supply, so there
// is nothing to discover afterwards — no startup-banner parsing, no port
// file, no environment variable the child publishes back to us. The
// alternative, --port 0 and reading the child's own log line for what it
// bound to, is exactly the discovery mechanism the issue found this
// runtime does not need.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("opencode: choosing a port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// generateCredential returns a fresh random Basic-auth password. Held only
// in memory by the caller — see Driver.password's doc comment for the full
// chain of custody the provider ruling on #55 requires.
func generateCredential() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("opencode: generating a credential: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// buildServeCmd builds the exec.Cmd for `opencode serve`, and is the one
// place that decides its argv and environment — factored out of
// startProcess so process_test.go can assert its shape directly (credential
// via env only, never argv; --mdns never present) against the real code
// path rather than a duplicate of it.
func buildServeCmd(bin, workdir string, port int, username, password string) *exec.Cmd {
	cmd := exec.Command(bin, "serve",
		"--port", strconv.Itoa(port),
		"--hostname", "127.0.0.1",
		// Deliberately no --mdns: it defaults the bind to 0.0.0.0
		// (measured on #55), which nothing here wants — this server is
		// reached only by this driver, over loopback.
	)
	if workdir != "" {
		cmd.Dir = workdir
	}
	// The credential travels through the ENVIRONMENT ONLY (Boss's provider
	// ruling on #55) — never as a command-line argument, which a process
	// table on the same machine can read, and never written to any file
	// this driver controls.
	cmd.Env = append(os.Environ(),
		"OPENCODE_SERVER_PASSWORD="+password,
		"OPENCODE_SERVER_USERNAME="+username,
	)
	// Discarded rather than captured: this driver's own diagnostics come
	// from the HTTP layer it talks to the server over, and capturing
	// stdout/stderr here would be one more place the credential could
	// theoretically be echoed back and retained (opencode does not do
	// this, but the discipline costs nothing and removes the question).
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd
}

// startProcess probes for the binary, then picks a port, generates a
// credential, execs `opencode serve`, and waits for the server to answer
// before returning. A failure at any step returns an error rather than
// panicking or calling log.Fatal — this package has no opinion on whether
// the absence of opencode should be fatal to its caller, and colab-fleetd
// (the only caller today) chooses "log and continue without this
// runtime", precisely so one optional third-party binary being missing
// never takes the whole fleet daemon down.
func startProcess(ctx context.Context, bin, workdir, username string) (*process, error) {
	avail := Probe(ctx, bin)
	if !avail.Installed {
		return nil, fmt.Errorf("opencode: not available on this machine: %w", avail.Err)
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	cred, err := generateCredential()
	if err != nil {
		return nil, err
	}

	cmd := buildServeCmd(avail.Path, workdir, port, username, cred)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("opencode: starting server: %w", err)
	}

	p := &process{
		cmd:      cmd,
		baseURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
		password: cred,
	}

	if err := waitReady(ctx, p.baseURL, username, cred); err != nil {
		_ = p.stop()
		return nil, fmt.Errorf("opencode: server did not become ready: %w", err)
	}
	return p, nil
}

// waitReady polls the server's own session listing (authenticated) until
// it answers or ctx / the readiness budget runs out.
func waitReady(ctx context.Context, baseURL, username, password string) error {
	cctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()

	for {
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, baseURL+"/session", nil)
		if err == nil {
			req.SetBasicAuth(username, password)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode < 500 {
					// Any non-5xx means the HTTP server is up and
					// applying auth (200 with the right credential; 401
					// would mean a credential mismatch, which is a bug
					// in this package, not "not ready" — either way the
					// process is answering).
					return nil
				}
			}
		}
		select {
		case <-cctx.Done():
			return cctx.Err()
		case <-ticker.C:
		}
	}
}

// stop terminates the child process. Idempotent-ish: calling it twice is
// harmless (Kill/Wait on an already-dead process just returns an error
// this method swallows).
func (p *process) stop() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
	return nil
}
