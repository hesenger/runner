// Command stubapp is a minimal app used by the e2e test as the binary that
// runner downloads, starts and swaps between versions.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// version is set at build time with -ldflags "-X main.version=v1".
var version = "dev"

// hangHealth set to "true" at build time makes /health never answer, so a
// health check only ends through the client's timeout.
var hangHealth = ""

func main() {
	port := flag.Int("port", 8080, "port to listen on")
	flag.Parse()

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if hangHealth == "true" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	// ?delay=1s holds the request open, simulating a slow in-flight request
	// that must still complete while runner drains this version.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if delay, err := time.ParseDuration(r.URL.Query().Get("delay")); err == nil {
			time.Sleep(delay)
		}
		fmt.Fprintf(w, "stubapp %s pid=%d port=%d\n", version, os.Getpid(), *port)
	})

	fmt.Printf("stubapp %s listening on :%d\n", version, *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}
