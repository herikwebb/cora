//go:build !windows

package process

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReviewerEnvironmentUsesAuthenticationAllowlist(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	t.Setenv("CORA_UNRELATED_SECRET", "must-not-leak")
	withoutBilling := strings.Join(ReviewerEnvironment(false), "\n")
	if strings.Contains(withoutBilling, "OPENAI_API_KEY=") || strings.Contains(withoutBilling, "CORA_UNRELATED_SECRET=") {
		t.Fatalf("reviewer environment leaked credentials:\n%s", withoutBilling)
	}
	withBilling := strings.Join(ReviewerEnvironment(true), "\n")
	if !strings.Contains(withBilling, "OPENAI_API_KEY=openai-secret") {
		t.Fatalf("billing opt-in did not preserve API credential:\n%s", withBilling)
	}
	if strings.Contains(withBilling, "CORA_UNRELATED_SECRET=") {
		t.Fatalf("billing opt-in leaked unrelated credential:\n%s", withBilling)
	}
}

func TestReviewerWorkspaceEnvironmentRedirectsWritableCaches(t *testing.T) {
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	values := make(map[string]string)
	for _, item := range ReviewerWorkspaceEnvironment(false, runtimeDir) {
		name, value, _ := strings.Cut(item, "=")
		values[name] = value
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP", "GOTMPDIR"} {
		if values[name] != runtimeDir {
			t.Fatalf("%s = %q, want %q", name, values[name], runtimeDir)
		}
	}
	for _, name := range []string{"GOCACHE", "XDG_CACHE_HOME", "npm_config_cache", "YARN_CACHE_FOLDER", "PYTHONPYCACHEPREFIX"} {
		if !strings.HasPrefix(values[name], runtimeDir+string(filepath.Separator)) {
			t.Fatalf("%s was not redirected into runtime: %q", name, values[name])
		}
	}
}

func TestRunCreatesPrivateOutputFilesWithPermissiveUmask(t *testing.T) {
	oldUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	outputDir := filepath.Join(t.TempDir(), "review-record")
	stdoutPath := filepath.Join(outputDir, "stdout.log")
	stderrPath := filepath.Join(outputDir, "stderr.log")
	result := Run(context.Background(), Spec{
		Command:    "sh",
		Args:       []string{"-c", "printf stdout; printf stderr >&2"},
		Env:        ReviewerEnvironment(false),
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
	})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	for path, want := range map[string]os.FileMode{
		outputDir:  0o700,
		stdoutPath: 0o600,
		stderrPath: 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("permissions for %s = %#o, want %#o", path, got, want)
		}
	}
}

func TestMinimalEnvironmentUsesAllowlistAndEphemeralDirectories(t *testing.T) {
	t.Setenv("CORA_ALLOWED_TEST_VALUE", "allowed")
	t.Setenv("CORA_SECRET_TEST_VALUE", "secret")
	t.Setenv("HOME", "/private/user-home")

	root := filepath.Join(t.TempDir(), "environment")
	environment, err := MinimalEnvironment(root, []string{"CORA_ALLOWED_TEST_VALUE"})
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string, len(environment))
	for _, item := range environment {
		name, value, _ := strings.Cut(item, "=")
		values[name] = value
	}
	if values["CORA_ALLOWED_TEST_VALUE"] != "allowed" {
		t.Fatalf("allowlisted value missing from environment: %v", values)
	}
	if _, found := values["CORA_SECRET_TEST_VALUE"]; found {
		t.Fatalf("secret value leaked into environment: %v", values)
	}
	if values["HOME"] != filepath.Join(root, "home") {
		t.Fatalf("HOME = %q, want ephemeral home", values["HOME"])
	}
	for _, name := range []string{"HOME", "TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"} {
		info, err := os.Stat(values[name])
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("permissions for %s = %#o, want 0700", name, got)
		}
	}
}

func TestRunTerminatesProcessGroupOnTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := Run(ctx, Spec{
		Command:    "sh",
		Args:       []string{"-c", "sleep 10"},
		Env:        ReviewerEnvironment(false),
		StdoutPath: filepath.Join(t.TempDir(), "stdout"),
		StderrPath: filepath.Join(t.TempDir(), "stderr"),
	})
	if result.Err == nil || !strings.Contains(result.Err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %#v", result)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout termination took %s", elapsed)
	}
}
