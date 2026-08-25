package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

type Spec struct {
	Command    string
	Args       []string
	Dir        string
	Stdin      []byte
	Env        []string
	StdoutPath string
	StderrPath string
}

type Result struct {
	ExitCode int
	Duration time.Duration
	Err      error
}

func Run(ctx context.Context, spec Spec) Result {
	started := time.Now()
	stdout, stdoutClose, err := outputWriter(spec.StdoutPath)
	if err != nil {
		return Result{ExitCode: -1, Duration: time.Since(started), Err: err}
	}
	defer stdoutClose()
	stderr, stderrClose, err := outputWriter(spec.StderrPath)
	if err != nil {
		return Result{ExitCode: -1, Duration: time.Since(started), Err: err}
	}
	defer stderrClose()

	command := exec.Command(spec.Command, spec.Args...)
	command.Dir = spec.Dir
	command.Env = spec.Env
	command.Stdin = bytes.NewReader(spec.Stdin)
	command.Stdout = stdout
	command.Stderr = stderr
	configureProcess(command)

	if err := command.Start(); err != nil {
		return Result{ExitCode: -1, Duration: time.Since(started), Err: fmt.Errorf("start %s: %w", spec.Command, err)}
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
	}()

	select {
	case err := <-done:
		return resultFromWait(command, started, err)
	case <-ctx.Done():
		terminateProcess(command)
		err := <-done
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{ExitCode: exitCode(command, err), Duration: time.Since(started), Err: fmt.Errorf("timed out: %w", ctx.Err())}
		}
		return Result{ExitCode: exitCode(command, err), Duration: time.Since(started), Err: ctx.Err()}
	}
}

func Capture(ctx context.Context, commandName, dir string, env []string, args ...string) ([]byte, []byte, Result) {
	started := time.Now()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := exec.Command(commandName, args...)
	command.Dir = dir
	command.Env = env
	command.Stdout = &stdout
	command.Stderr = &stderr
	configureProcess(command)
	if err := command.Start(); err != nil {
		return nil, nil, Result{ExitCode: -1, Duration: time.Since(started), Err: err}
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return stdout.Bytes(), stderr.Bytes(), resultFromWait(command, started, err)
	case <-ctx.Done():
		terminateProcess(command)
		err := <-done
		return stdout.Bytes(), stderr.Bytes(), Result{ExitCode: exitCode(command, err), Duration: time.Since(started), Err: ctx.Err()}
	}
}

// ReviewerEnvironment preserves only the environment needed to locate the
// reviewer CLIs and reuse their existing local authentication. Unrelated
// session variables and credentials are not exposed to reviewer subprocesses.
func ReviewerEnvironment(allowAPIBilling bool) []string {
	names := []string{
		"PATH", "PATHEXT", "SYSTEMROOT", "COMSPEC", "WINDIR",
		"HOME", "USERPROFILE", "USER", "LOGNAME", "SHELL",
		"TMPDIR", "TEMP", "TMP",
		"LANG", "LC_ALL", "LC_CTYPE", "TERM",
		"CODEX_HOME", "CLAUDE_CONFIG_DIR",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME",
		"APPDATA", "LOCALAPPDATA",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
	}
	if allowAPIBilling {
		names = append(names, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CODEX_API_KEY", "OPENAI_API_KEY")
	}
	values := make(map[string]string, len(names))
	for _, name := range names {
		if value, found := os.LookupEnv(name); found {
			values[name] = value
		}
	}
	sort.Strings(names)
	environment := make([]string, 0, len(values))
	for _, name := range names {
		if value, found := values[name]; found {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

// MinimalEnvironment returns an environment suitable for an explicitly
// allowed host check. It starts from an allowlist and replaces user-specific
// home, cache, config, and temporary directories with private empty ones.
func MinimalEnvironment(root string, allowlist []string) ([]string, error) {
	directories := map[string]string{
		"HOME":            filepath.Join(root, "home"),
		"USERPROFILE":     filepath.Join(root, "home"),
		"TMPDIR":          filepath.Join(root, "tmp"),
		"TEMP":            filepath.Join(root, "tmp"),
		"TMP":             filepath.Join(root, "tmp"),
		"XDG_CACHE_HOME":  filepath.Join(root, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
		"APPDATA":         filepath.Join(root, "config"),
		"LOCALAPPDATA":    filepath.Join(root, "data"),
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create minimal environment root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure minimal environment root: %w", err)
	}
	for _, path := range directories {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("create minimal environment directory: %w", err)
		}
	}

	values := make(map[string]string)
	for _, name := range []string{"PATH", "PATHEXT", "SYSTEMROOT", "COMSPEC", "WINDIR", "LANG", "LC_ALL", "LC_CTYPE"} {
		if value, found := os.LookupEnv(name); found {
			values[name] = value
		}
	}
	for _, name := range allowlist {
		if value, found := os.LookupEnv(name); found {
			values[name] = value
		}
	}
	for name, value := range directories {
		values[name] = value
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment, nil
}

func resultFromWait(command *exec.Cmd, started time.Time, err error) Result {
	result := Result{ExitCode: exitCode(command, err), Duration: time.Since(started)}
	if err != nil {
		result.Err = err
	}
	return result
}

func exitCode(command *exec.Cmd, err error) int {
	if command.ProcessState != nil {
		return command.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func outputWriter(path string) (io.Writer, func(), error) {
	if path == "" {
		return io.Discard, func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, func() {}, fmt.Errorf("create output directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, func() {}, fmt.Errorf("secure output directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, func() {}, fmt.Errorf("create output file %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, func() {}, fmt.Errorf("secure output file %s: %w", path, err)
	}
	return file, func() { _ = file.Close() }, nil
}
