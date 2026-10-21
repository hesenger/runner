// Command fakegithub imitates the small part of the GitHub Actions artifacts
// API that runner uses, serving zips from a local directory.
//
//	GET  /repos/{owner}/{repo}/actions/artifacts[?name=&per_page=]
//	     published artifacts, newest first
//	GET  /download/{id}  zip of a published artifact (410 when expired)
//	POST /publish?file=<zip>&id=<n>[&name=<s>][&branch=<b>][&expired=true]
//	     publish <dir>/<zip> as the newest artifact (branch defaults to main)
//	GET  /stats[?id=<n>]  download counts: JSON for all ids, or a plain number
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type workflowRun struct {
	HeadBranch string `json:"head_branch"`
}

type artifact struct {
	ID                 int64       `json:"id"`
	Name               string      `json:"name"`
	ArchiveDownloadURL string      `json:"archive_download_url"`
	CreatedAt          string      `json:"created_at"`
	Expired            bool        `json:"expired"`
	WorkflowRun        workflowRun `json:"workflow_run"`

	file string
}

type server struct {
	dir string

	mu        sync.Mutex
	artifacts []artifact // newest first
	downloads map[int64]int
}

func main() {
	port := flag.Int("port", 18100, "port to listen on")
	dir := flag.String("dir", ".", "directory containing artifact zips")
	flag.Parse()

	s := &server{dir: *dir, downloads: map[int64]int{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/artifacts", s.listArtifacts)
	mux.HandleFunc("GET /download/{id}", s.download)
	mux.HandleFunc("POST /publish", s.publish)
	mux.HandleFunc("GET /stats", s.stats)

	fmt.Printf("[fakegithub] serving %s on :%d\n", *dir, *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), mux))
}

func (s *server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	perPage, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || perPage <= 0 {
		perPage = 30
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	artifacts := []artifact{}
	for _, a := range s.artifacts {
		if name != "" && a.Name != name {
			continue
		}
		if len(artifacts) == perPage {
			break
		}
		a.ArchiveDownloadURL = fmt.Sprintf("http://%s/download/%d", r.Host, a.ID)
		artifacts = append(artifacts, a)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"total_count": len(artifacts),
		"artifacts":   artifacts,
	})
}

func (s *server) find(id int64) (artifact, bool) {
	for _, a := range s.artifacts {
		if a.ID == id {
			return a, true
		}
	}
	return artifact{}, false
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.downloads[id]++
	a, ok := s.find(id)
	s.mu.Unlock()

	fmt.Printf("[fakegithub] download %d\n", id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if a.Expired {
		http.Error(w, "artifact has expired", http.StatusGone)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	http.ServeFile(w, r, filepath.Join(s.dir, a.file))
}

func (s *server) publish(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	file := filepath.Base(q.Get("file"))
	id, err := strconv.ParseInt(q.Get("id"), 10, 64)
	if file == "." || file == "/" || err != nil {
		http.Error(w, "usage: POST /publish?file=<zip>&id=<n>[&name=<s>][&branch=<b>][&expired=true]", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(filepath.Join(s.dir, file)); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	name := q.Get("name")
	if name == "" {
		name = strings.TrimSuffix(file, filepath.Ext(file))
	}
	branch := q.Get("branch")
	if branch == "" {
		branch = "main"
	}
	expired := q.Get("expired") == "true"

	a := artifact{
		ID:          id,
		Name:        name,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Expired:     expired,
		WorkflowRun: workflowRun{HeadBranch: branch},
		file:        file,
	}

	s.mu.Lock()
	s.artifacts = append([]artifact{a}, s.artifacts...)
	s.mu.Unlock()

	fmt.Printf("[fakegithub] published %s as artifact %d (name=%s branch=%s expired=%v)\n", file, id, name, branch, expired)
	fmt.Fprintf(w, "published %s as artifact %d\n", file, id)
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idParam := r.URL.Query().Get("id"); idParam != "" {
		id, err := strconv.ParseInt(idParam, 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "%d\n", s.downloads[id])
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"downloads": s.downloads})
}
