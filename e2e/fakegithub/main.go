// Command fakegithub imitates the small part of the GitHub Actions artifacts
// API that runner uses, serving zips from a local directory.
//
//	GET  /repos/{owner}/{repo}/actions/artifacts[?name=&per_page=]
//	     published artifacts, newest first
//	GET  /download/{id}  zip of a published artifact (410 when expired)
//	POST /publish?file=<zip>&id=<n>[&name=<s>][&branch=<b>][&expired=true]
//	     publish <dir>/<zip> as the newest artifact (branch defaults to main)
//	GET  /stats[?id=<n>]  download counts: JSON for all ids, or a plain number
//	GET  /token?repo=<owner/name>  token of the last artifacts list of that repo
package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type workflowRun struct {
	ID         int64  `json:"id"`
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

	mu sync.Mutex
	// artifacts are listed by artifact ID, highest first, like GitHub does
	artifacts []artifact
	// lastRun is the run ID of the last published artifact
	lastRun   int64
	downloads map[int64]int
	// tokens is the token of the last artifacts list, by owner/name
	tokens map[string]string
}

func main() {
	port := flag.Int("port", 18100, "port to listen on")
	dir := flag.String("dir", ".", "directory containing artifact zips")
	flag.Parse()

	// runs start at 100, so a publish can pass an older run below them
	s := &server{dir: *dir, downloads: map[int64]int{}, tokens: map[string]string{}, lastRun: 99}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/artifacts", s.listArtifacts)
	mux.HandleFunc("GET /download/{id}", s.download)
	mux.HandleFunc("POST /publish", s.publish)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("GET /token", s.token)

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

	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	s.tokens[repo] = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

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
		http.Error(w, "usage: POST /publish?file=<zip>&id=<n>[&name=<s>][&branch=<b>][&run=<n>][&expired=true]", http.StatusBadRequest)
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

	s.mu.Lock()
	// each publish is a new run unless ?run= names an older one, like a run
	// that was triggered earlier but finished later
	run, err := strconv.ParseInt(q.Get("run"), 10, 64)
	if err != nil {
		s.lastRun++
		run = s.lastRun
	}
	a := artifact{
		ID:          id,
		Name:        name,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Expired:     expired,
		WorkflowRun: workflowRun{ID: run, HeadBranch: branch},
		file:        file,
	}
	i, _ := slices.BinarySearchFunc(s.artifacts, id, func(a artifact, id int64) int { return cmp.Compare(id, a.ID) })
	s.artifacts = slices.Insert(s.artifacts, i, a)
	s.mu.Unlock()

	fmt.Printf("[fakegithub] published %s as artifact %d (name=%s branch=%s run=%d expired=%v)\n", file, id, name, branch, run, expired)
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

func (s *server) token(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(w, "%s\n", s.tokens[r.URL.Query().Get("repo")])
}
