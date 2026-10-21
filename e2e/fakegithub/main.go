// Command fakegithub imitates the small part of the GitHub Actions artifacts
// API that runner uses, serving zips from a local directory.
//
//	GET  /repos/{owner}/{repo}/actions/artifacts  latest published artifact
//	GET  /download/{id}                           zip of a published artifact
//	POST /publish?file=<zip>&id=<n>[&name=<s>]    make <dir>/<zip> the latest artifact
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

type artifact struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	ArchiveDownloadURL string `json:"archive_download_url"`
	CreatedAt          string `json:"created_at"`

	file string
}

type server struct {
	dir string

	mu        sync.Mutex
	published map[int64]artifact
	latest    *artifact
}

func main() {
	port := flag.Int("port", 18100, "port to listen on")
	dir := flag.String("dir", ".", "directory containing artifact zips")
	flag.Parse()

	s := &server{dir: *dir, published: map[int64]artifact{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/artifacts", s.listArtifacts)
	mux.HandleFunc("GET /download/{id}", s.download)
	mux.HandleFunc("POST /publish", s.publish)

	fmt.Printf("[fakegithub] serving %s on :%d\n", *dir, *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), mux))
}

func (s *server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	artifacts := []artifact{}
	if s.latest != nil {
		a := *s.latest
		a.ArchiveDownloadURL = fmt.Sprintf("http://%s/download/%d", r.Host, a.ID)
		artifacts = append(artifacts, a)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"total_count": len(artifacts),
		"artifacts":   artifacts,
	})
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	a, ok := s.published[id]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	http.ServeFile(w, r, filepath.Join(s.dir, a.file))
}

func (s *server) publish(w http.ResponseWriter, r *http.Request) {
	file := filepath.Base(r.URL.Query().Get("file"))
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if file == "." || file == "/" || err != nil {
		http.Error(w, "usage: POST /publish?file=<zip>&id=<n>[&name=<name>]", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(filepath.Join(s.dir, file)); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		name = strings.TrimSuffix(file, filepath.Ext(file))
	}

	a := artifact{ID: id, Name: name, CreatedAt: time.Now().UTC().Format(time.RFC3339), file: file}

	s.mu.Lock()
	s.published[id] = a
	s.latest = &a
	s.mu.Unlock()

	fmt.Printf("[fakegithub] published %s as artifact %d (%s)\n", file, id, name)
	fmt.Fprintf(w, "published %s as artifact %d\n", file, id)
}
