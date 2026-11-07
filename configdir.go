package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Limits of a configDir, so a wrong path such as a home directory is
// refused instead of hashed on every reload tick.
const (
	maxConfigFiles = 1000
	maxConfigBytes = 10 << 20
)

// configEntry is a regular file of a configDir.
type configEntry struct {
	// rel is the path relative to the configDir, with forward slashes
	rel  string
	info fs.FileInfo
}

// configIgnored reports whether rel, a path relative to the configDir with
// forward slashes, matches one of the configIgnore patterns: a pattern ending
// in "/" matches that directory and everything in it, any other pattern is
// matched against the whole relative path with path.Match.
func configIgnored(rel string, ignore []string) bool {
	for _, p := range ignore {
		if dir, ok := strings.CutSuffix(p, "/"); ok {
			if rel == dir || strings.HasPrefix(rel, p) {
				return true
			}
			if matched, _ := path.Match(dir, rel); matched {
				return true
			}
			continue
		}
		if matched, _ := path.Match(p, rel); matched {
			return true
		}
	}
	return false
}

// validateConfigIgnore checks one configIgnore pattern.
func validateConfigIgnore(p string) error {
	dir := strings.TrimSuffix(p, "/")
	switch {
	case dir == "":
		return fmt.Errorf("pattern must not be empty")
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("pattern %q must be relative to configDir", p)
	case slices.Contains(strings.Split(dir, "/"), ".."):
		return fmt.Errorf("pattern %q must not contain ..", p)
	}
	if _, err := path.Match(dir, ""); err != nil {
		return fmt.Errorf("pattern %q: %w", p, err)
	}
	return nil
}

// listConfigDir returns the regular files of dir minus the ignored ones,
// sorted by relative path. Symlinks and other special files are left out. It
// fails beyond maxConfigFiles files or maxConfigBytes in total.
func listConfigDir(dir string, ignore []string) ([]configEntry, error) {
	var entries []configEntry
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if configIgnored(rel, ignore) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entries = append(entries, configEntry{rel: rel, info: info})
		total += info.Size()
		if len(entries) > maxConfigFiles {
			return fmt.Errorf("more than %d files, add the ones the app writes to configIgnore", maxConfigFiles)
		}
		if total > maxConfigBytes {
			return fmt.Errorf("more than %d MB, add the files the app writes to configIgnore", maxConfigBytes>>20)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b configEntry) int { return strings.Compare(a.rel, b.rel) })
	return entries, nil
}

// dirSignature is a cheap fingerprint of dir from file metadata only: path,
// size, modification time and mode of every file that is not ignored.
func dirSignature(dir string, ignore []string) (string, error) {
	entries, err := listConfigDir(dir, ignore)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%o\n", e.rel, e.info.Size(), e.info.ModTime().UnixNano(), e.info.Mode())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// dirContentHash is a SHA-256 over the relative path and the SHA-256 of the
// content of every file of dir that is not ignored. files maps each relative
// path to its own hash, to tell which files changed.
func dirContentHash(dir string, ignore []string) (hash string, files map[string]string, err error) {
	entries, err := listConfigDir(dir, ignore)
	if err != nil {
		return "", nil, err
	}
	files = map[string]string{}
	h := sha256.New()
	for _, e := range entries {
		sum, err := fileSHA256(filepath.Join(dir, filepath.FromSlash(e.rel)))
		if err != nil {
			return "", nil, err
		}
		files[e.rel] = sum
		fmt.Fprintf(h, "%s\x00%s\n", e.rel, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), files, nil
}

// fileSHA256 returns the hex SHA-256 of a file's content.
func fileSHA256(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// changedFiles returns the sorted relative paths added, removed or changed
// between two files maps of dirContentHash.
func changedFiles(a, b map[string]string) []string {
	var names []string
	for name, sum := range a {
		if b[name] != sum {
			names = append(names, name)
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// describeFiles lists file names for the log, at most 5 of them.
func describeFiles(names []string) string {
	if len(names) > 5 {
		return strings.Join(names[:5], ", ") + fmt.Sprintf(" and %d more", len(names)-5)
	}
	return strings.Join(names, ", ")
}

// configDirState is what the reload loop knows of an app's configDir.
type configDirState struct {
	// signature, hash and files are of the content last seen
	signature string
	hash      string
	files     map[string]string
	// applied is the hash the app's loop was last sent, which may differ
	// from hash when the loop guard skipped a change
	applied string
	// redeployed is set when a change was sent to the app's loop, until the
	// next tick
	redeployed bool
	// err is the last error reading the dir, so it is logged once
	err string
}

// readConfigDirState reads the current content of app's configDir.
func readConfigDirState(app App) (*configDirState, time.Time, error) {
	st := &configDirState{}
	sig, err := dirSignature(app.ConfigDir, app.ConfigIgnore)
	if err != nil {
		st.err = err.Error()
		return st, time.Time{}, err
	}
	hash, files, err := dirContentHash(app.ConfigDir, app.ConfigIgnore)
	if err != nil {
		st.err = err.Error()
		return st, time.Time{}, err
	}
	st.signature, st.hash, st.files, st.applied = sig, hash, files, hash
	// the age shown is of the newest file
	var newest time.Time
	entries, _ := listConfigDir(app.ConfigDir, app.ConfigIgnore)
	for _, e := range entries {
		if t := e.info.ModTime(); t.After(newest) {
			newest = t
		}
	}
	return st, newest, nil
}

// shortHash is the first 8 hex digits of a hash, as shown in the admin UI.
func shortHash(hash string) string {
	return hash[:min(8, len(hash))]
}

// configDirReady reports why an app cannot start in dir, nil when it can.
func configDirReady(dir string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}
