package provider

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// prepareGeminiSource copies a disposable review tree without Git metadata or
// filenames that Gemini automatically interprets as instructions. Its mapping
// contains only renamed paths, relative to each tree and using slash separators.
// The caller owns the destination and removes it after either success or failure.
// Symlinks fail closed: following or rewriting them would either expose files
// outside the snapshot or silently change the source semantics being reviewed.
func prepareGeminiSource(sourceRoot, destinationRoot string) (map[string]string, error) {
	sourcePath, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve Gemini review source: %w", err)
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return nil, err
	}
	destinationPath, err := filepath.Abs(destinationRoot)
	if err != nil {
		return nil, err
	}
	// Resolve the existing parent before creating anything, including when a
	// parent symlink would otherwise place the destination inside the source.
	destinationParent, err := filepath.EvalSymlinks(filepath.Dir(destinationPath))
	if err != nil {
		return nil, err
	}
	destinationPath = filepath.Join(destinationParent, filepath.Base(destinationPath))
	relativeDestination, err := filepath.Rel(sourcePath, destinationPath)
	if err != nil {
		return nil, err
	}
	if relativeDestination != ".." && !strings.HasPrefix(relativeDestination, ".."+string(filepath.Separator)) && !filepath.IsAbs(relativeDestination) {
		return nil, errors.New("Gemini source mirror destination must be outside the source tree")
	}
	if err := os.Mkdir(destinationPath, 0o700); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("create Gemini source mirror: %w", err)
	}
	destinationInfo, err := os.Lstat(destinationPath)
	if err != nil {
		return nil, err
	}
	if !destinationInfo.IsDir() {
		return nil, errors.New("Gemini source mirror destination must be a directory, not a symlink")
	}
	existing, err := os.ReadDir(destinationPath)
	if err != nil {
		return nil, err
	}
	if len(existing) != 0 {
		return nil, errors.New("Gemini source mirror destination must be empty")
	}
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("open Gemini review source: %w", err)
	}
	defer source.Close()
	type sourceEntry struct {
		name string
		info fs.FileInfo
	}
	var entries []sourceEntry
	occupied := make(map[string]bool)
	err = fs.WalkDir(source.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if strings.EqualFold(name, ".git") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Gemini source mirror cannot safely review symlink %q; use another reviewer for this target", name)
		}
		if entry.IsDir() && strings.EqualFold(entry.Name(), "GEMINI.md") {
			return fmt.Errorf("Gemini source mirror cannot safely review directory %q named GEMINI.md", name)
		}
		if !entry.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("Gemini source mirror cannot review non-regular file %q", name)
		}
		entries = append(entries, sourceEntry{name, info})
		occupied[strings.ToLower(name)] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	mapping := make(map[string]string)
	for _, entry := range entries {
		if !strings.EqualFold(path.Base(entry.name), "GEMINI.md") {
			continue
		}
		for {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return nil, fmt.Errorf("name Gemini instruction evidence: %w", err)
			}
			alias := path.Join(path.Dir(entry.name), ".cora-gemini-context-"+hex.EncodeToString(random[:])+".txt")
			if occupied[strings.ToLower(alias)] {
				continue
			}
			occupied[strings.ToLower(alias)] = true
			mapping[entry.name] = alias
			break
		}
	}
	destination, err := os.OpenRoot(destinationPath)
	if err != nil {
		return nil, err
	}
	defer destination.Close()
	for _, entry := range entries {
		if entry.info.IsDir() {
			if err := destination.Mkdir(entry.name, 0o700); err != nil {
				return nil, err
			}
			continue
		}
		name := entry.name
		if alias, ok := mapping[name]; ok {
			name = alias
		}
		if err := copyGeminiSourceFile(source, destination, entry.name, name, entry.info); err != nil {
			return nil, fmt.Errorf("copy Gemini source %q: %w", entry.name, err)
		}
	}
	// Restore directory permissions after creating their children.
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.info.IsDir() {
			if err := destination.Chmod(entry.name, entry.info.Mode().Perm()); err != nil {
				return nil, err
			}
		}
	}
	return mapping, nil
}

func copyGeminiSourceFile(source, destination *os.Root, sourceName, destinationName string, expected fs.FileInfo) error {
	input, err := source.Open(sourceName)
	if err != nil {
		return err
	}
	defer input.Close()
	actual, err := input.Stat()
	if err != nil {
		return err
	}
	if !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
		return errors.New("source changed while preparing Gemini review")
	}
	output, err := destination.OpenFile(destinationName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	modeErr := output.Chmod(expected.Mode().Perm())
	closeErr := output.Close()
	return errors.Join(copyErr, modeErr, closeErr)
}
