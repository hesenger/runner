package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeZip creates dir/name holding one entry per name in entries, in order.
func writeZip(t *testing.T, dir, name string, entries ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, entry := range entries {
		ew, err := w.Create(entry)
		if err != nil {
			t.Fatal(err)
		}
		ew.Write([]byte("content of " + entry))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertCleanedUp checks a failed extraction left neither the zip nor its dir.
func assertCleanedUp(t *testing.T, zipPath, extractionDir string) {
	t.Helper()
	if _, err := os.Stat(zipPath); !os.IsNotExist(err) {
		t.Errorf("zip %s still exists after a failed extraction (stat err: %v)", zipPath, err)
	}
	if _, err := os.Stat(extractionDir); !os.IsNotExist(err) {
		t.Errorf("extraction dir %s still exists after a failed extraction (stat err: %v)", extractionDir, err)
	}
}

func TestDecompressArtifact(t *testing.T) {
	dir := t.TempDir()
	zipPath := writeZip(t, dir, "app-1.zip", "app", "..config", "sub/file")
	binDir := filepath.Join(dir, "bin")

	path, err := DecompressArtifact(zipPath, binDir)
	if err != nil {
		t.Fatalf("DecompressArtifact: %v", err)
	}
	if want := filepath.Join(binDir, "app-1"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	for _, name := range []string{"app", "..config", "sub/file"} {
		got, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			t.Errorf("%s not extracted: %v", name, err)
		} else if string(got) != "content of "+name {
			t.Errorf("%s content = %q", name, got)
		}
	}
	if _, err := os.Stat(zipPath); !os.IsNotExist(err) {
		t.Errorf("zip still exists after a successful extraction (stat err: %v)", err)
	}
}

func TestDecompressArtifactRejectsNonLocalPaths(t *testing.T) {
	for _, entry := range []string{"../evil", "sub/../../evil", "/etc/evil"} {
		t.Run(entry, func(t *testing.T) {
			dir := t.TempDir()
			// a valid entry first, so the extraction dir exists when the bad one is hit
			zipPath := writeZip(t, dir, "app-1.zip", "app", entry)
			binDir := filepath.Join(dir, "bin")

			_, err := DecompressArtifact(zipPath, binDir)
			if err == nil || !strings.Contains(err.Error(), "directory traversal") {
				t.Fatalf("err = %v, want a directory traversal error", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "evil")); !os.IsNotExist(err) {
				t.Errorf("entry %q was written outside the extraction dir", entry)
			}
			assertCleanedUp(t, zipPath, filepath.Join(binDir, "app-1"))
		})
	}
}

func TestDecompressArtifactCleansUpInvalidZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "app-1.zip")
	if err := os.WriteFile(zipPath, []byte("not a zip"), 0644); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")

	if _, err := DecompressArtifact(zipPath, binDir); err == nil {
		t.Fatal("DecompressArtifact succeeded on an invalid zip")
	}
	assertCleanedUp(t, zipPath, filepath.Join(binDir, "app-1"))
}
