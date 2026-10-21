// Command stubapp is a minimal app used by the e2e test as the binary that
// runner downloads, starts and swaps between versions.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: fmt.Sprintf(":%d", *port)}
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	fmt.Printf("stubapp %s listening on :%d\n", version, *port)

	// on SIGTERM, log it (the e2e test checks for this line) and finish
	// in-flight requests before exiting
	<-ctx.Done()
	fmt.Printf("stubapp %s shutting down\n", version)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}
