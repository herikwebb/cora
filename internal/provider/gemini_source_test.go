package provider

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrepareGeminiSourcePreservesEvidenceWithoutLoadingInstructions(t *testing.T) {
	source, destination := t.TempDir(), filepath.Join(t.TempDir(), "mirror")
	files := map[string][]byte{
		"GEMINI.md":              []byte("root instructions\n@secret.txt\n"),
		"nested/gEmInI.Md":       []byte("nested instructions\n"),
		"nested/other/GEMINI.md": []byte{0, 1, 2, 255},
		"nested/script.sh":       []byte("#!/bin/sh\nexit 1\n"),
		".gemini/settings.json":  []byte(`{"hooks":{"SessionStart":[{"command":"unsafe"}]}}`),
		".cora-gemini-context-00000000000000000000000000000000.txt": []byte("existing evidence namespace\n"),
		"nested/.cora-gemini-context-existing.txt":                  []byte("existing nested evidence\n"),
		".git/GEMINI.md": []byte("must not be copied\n"),
	}
	for name, contents := range files {
		writeGeminiSourceFixture(t, source, name, contents, 0o644)
	}
	if err := os.Chmod(filepath.Join(source, "nested/script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	mapping, err := prepareGeminiSource(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 3 {
		t.Fatalf("mapping = %#v, want three renamed instruction files", mapping)
	}
	for name, contents := range files {
		original, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(original, contents) {
			t.Fatalf("original %q changed: contents=%q error=%v", name, original, err)
		}
		if strings.HasPrefix(name, ".git/") {
			continue
		}
		mirrorName := name
		if alias, ok := mapping[name]; ok {
			mirrorName = alias
			if strings.Contains(alias, "\\") || filepath.Dir(alias) != filepath.Dir(name) || strings.EqualFold(filepath.Base(alias), "GEMINI.md") {
				t.Fatalf("unsafe alias %q for %q", alias, name)
			}
			if _, err := os.Lstat(filepath.Join(destination, filepath.FromSlash(name))); !os.IsNotExist(err) {
				t.Fatalf("instruction filename %q remains: %v", name, err)
			}
		}
		mirrored, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(mirrorName)))
		if err != nil || !bytes.Equal(mirrored, contents) {
			t.Fatalf("mirrored %q changed: contents=%q error=%v", name, mirrored, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(destination, ".git")); !os.IsNotExist(err) {
		t.Fatalf("Git metadata remains: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(destination, "nested/script.sh"))
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("executable mode not preserved: info=%v error=%v", info, err)
		}
	}
	if err := filepath.WalkDir(destination, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.EqualFold(entry.Name(), "GEMINI.md") {
			t.Errorf("automatic instruction entry remains: %s", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	second, err := prepareGeminiSource(source, filepath.Join(t.TempDir(), "mirror"))
	if err != nil {
		t.Fatal(err)
	}
	for original, alias := range mapping {
		if second[original] == alias {
			t.Fatalf("instruction alias is predictable across runs: %q", alias)
		}
	}
}

func TestPrepareGeminiSourceRejectsSymlinksWithoutFollowingTargets(t *testing.T) {
	for _, kind := range []string{"external-file", "external-directory", "internal-file", "dangling", "instruction-name"} {
		t.Run(kind, func(t *testing.T) {
			source, destination, outside := t.TempDir(), t.TempDir(), t.TempDir()
			writeGeminiSourceFixture(t, source, "plain.txt", []byte("inside"), 0o644)
			secretPath := filepath.Join(outside, "GEMINI.md")
			secret := []byte("outside instruction and private data")
			if err := os.WriteFile(secretPath, secret, 0o600); err != nil {
				t.Fatal(err)
			}
			name, target := "link", secretPath
			switch kind {
			case "external-directory":
				target = outside
			case "internal-file":
				target = "plain.txt"
			case "dangling":
				target = filepath.Join(outside, "missing")
			case "instruction-name":
				name = "gEmInI.Md"
			}
			if err := os.Symlink(target, filepath.Join(source, name)); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink creation unavailable: %v", err)
				}
				t.Fatal(err)
			}
			mapping, err := prepareGeminiSource(source, destination)
			if err == nil || !strings.Contains(err.Error(), "cannot safely review symlink") || mapping != nil {
				t.Fatalf("symlink accepted: mapping=%#v error=%v", mapping, err)
			}
			if strings.Contains(err.Error(), string(secret)) {
				t.Fatal("error disclosed outside content")
			}
			entries, err := os.ReadDir(destination)
			if err != nil || len(entries) != 0 {
				t.Fatalf("source copied before symlink validation: entries=%v error=%v", entries, err)
			}
			if actual, err := os.Readlink(filepath.Join(source, name)); err != nil || actual != target {
				t.Fatalf("original symlink changed: target=%q error=%v", actual, err)
			}
		})
	}
}

func TestPrepareGeminiSourceRejectsInstructionDirectories(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "gEmInI.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareGeminiSource(source, destination); err == nil || !strings.Contains(err.Error(), "named GEMINI.md") {
		t.Fatalf("instruction directory accepted: %v", err)
	}
}

func TestPrepareGeminiSourceRejectsUnsafeDestination(t *testing.T) {
	source := t.TempDir()
	writeGeminiSourceFixture(t, source, "plain.txt", []byte("original"), 0o644)
	for _, destination := range []string{source, filepath.Join(source, "mirror")} {
		if _, err := prepareGeminiSource(source, destination); err == nil {
			t.Fatalf("source-contained destination accepted: %q", destination)
		}
	}
	if _, err := os.Lstat(filepath.Join(source, "mirror")); !os.IsNotExist(err) {
		t.Fatalf("unsafe destination changed source tree: %v", err)
	}
	destination := t.TempDir()
	writeGeminiSourceFixture(t, destination, "existing.txt", []byte("do not overwrite"), 0o644)
	if _, err := prepareGeminiSource(source, destination); err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("nonempty destination accepted: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "existing.txt"))
	if err != nil || string(contents) != "do not overwrite" {
		t.Fatalf("destination content changed: contents=%q error=%v", contents, err)
	}
}

func writeGeminiSourceFixture(t *testing.T, root, name string, contents []byte, mode fs.FileMode) {
	t.Helper()
	filePath := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, contents, mode); err != nil {
		t.Fatal(err)
	}
}
