// Command demoapp is the chaos target for the e2e tests.
//
//	GET  /healthz  200 while healthy, 503 once broken
//	POST /break    break this process (state is in memory, so a restart fixes it)
//	POST /heal     undo /break
//
// FORCE_FAIL=1 makes it unhealthy from startup, so restarts cannot fix it;
// that is how the e2e test forces escalation to failover.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
)

func main() {
	var broken atomic.Bool
	broken.Store(os.Getenv("FORCE_FAIL") == "1")
	track := os.Getenv("TRACK")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if broken.Load() {
			http.Error(w, "broken "+track, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "ok %s\n", track)
	})
	mux.HandleFunc("POST /break", func(w http.ResponseWriter, _ *http.Request) { broken.Store(true) })
	mux.HandleFunc("POST /heal", func(w http.ResponseWriter, _ *http.Request) { broken.Store(false) })

	addr := ":8080"
	log.Printf("demoapp track=%s listening on %s (broken=%v)", track, addr, broken.Load())
	log.Fatal(http.ListenAndServe(addr, mux))
}
