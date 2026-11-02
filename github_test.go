package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func testArtifact(id, runID int64, name, branch string, expired bool) Artifact {
	a := Artifact{ID: id, Name: name, Expired: expired}
	a.WorkflowRun.ID = runID
	a.WorkflowRun.HeadBranch = branch
	return a
}

func TestPickArtifact(t *testing.T) {
	artifacts := []Artifact{
		testArtifact(5, 50, "app", "main", true),
		testArtifact(4, 40, "app", "feature", false),
		testArtifact(3, 30, "docs", "main", false),
		testArtifact(2, 20, "app", "main", false),
		testArtifact(1, 10, "app", "main", false),
	}
	// listed by artifact ID, but the runs overlapped: run 30 was triggered
	// last and its artifact got the lowest ID
	overlapping := []Artifact{
		testArtifact(13147365145, 10, "app", "main", false),
		testArtifact(13146568124, 20, "app", "main", false),
		testArtifact(13146284121, 30, "app", "main", false),
	}
	// two artifacts of one run, and artifacts without run IDs
	sameRun := []Artifact{
		testArtifact(7, 20, "app", "main", false),
		testArtifact(9, 20, "app", "main", false),
		testArtifact(8, 10, "app", "main", false),
	}
	noRunIDs := []Artifact{
		testArtifact(2, 0, "app", "main", false),
		testArtifact(3, 0, "app", "main", false),
	}

	tests := []struct {
		name      string
		artifacts []Artifact
		artName   string
		branch    string
		wantID    int64
		wantOK    bool
	}{
		{"empty list", nil, "", "", 0, false},
		{"no filters skips expired", artifacts, "", "", 4, true},
		{"branch filter", artifacts, "", "main", 3, true},
		{"name filter", artifacts, "app", "", 4, true},
		{"name and branch", artifacts, "app", "main", 2, true},
		{"branch mismatch", artifacts, "", "release", 0, false},
		{"name mismatch", artifacts, "web", "", 0, false},
		{"all expired", []Artifact{testArtifact(1, 10, "app", "main", true)}, "", "", 0, false},
		{"latest run wins over artifact order", overlapping, "app", "main", 13146284121, true},
		{"same run, highest artifact ID", sameRun, "app", "main", 9, true},
		{"no run IDs, highest artifact ID", noRunIDs, "app", "main", 3, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pickArtifact(tt.artifacts, tt.artName, tt.branch)
			if ok != tt.wantOK || got.ID != tt.wantID {
				t.Errorf("pickArtifact() = (%d, %v), want (%d, %v)", got.ID, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}

func TestDownloadArtifactRejectsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "artifact expired", http.StatusGone)
	}))
	defer srv.Close()

	dir := t.TempDir()
	target := Artifact{ID: 7, Name: "app", ArchiveDownloadURL: srv.URL}
	if _, err := DownloadArtifact(http.DefaultClient, target, "token", dir); err == nil {
		t.Fatal("expected an error for a 410 response")
	}
	if _, err := os.Stat(filepath.Join(dir, "app-7.zip")); !os.IsNotExist(err) {
		t.Errorf("error page was saved as a zip (stat err: %v)", err)
	}
}

func TestDownloadArtifactSavesZip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("zip-bytes"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	target := Artifact{ID: 7, Name: "app", ArchiveDownloadURL: srv.URL}
	path, err := DownloadArtifact(http.DefaultClient, target, "token", dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "app-7.zip"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	if data, _ := os.ReadFile(path); string(data) != "zip-bytes" {
		t.Errorf("saved %q, want %q", data, "zip-bytes")
	}
}
