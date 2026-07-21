package main

import (
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
)

// startPprofServer starts an opt-in diagnostic server on an isolated mux.
// The address must resolve to loopback so profiling can never be exposed by
// the public application listener accidentally.
func startPprofServer() {
	addr := strings.TrimSpace(os.Getenv("PPROF_ADDR"))
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		log.Printf("pprof disabled: invalid PPROF_ADDR: %v", err)
		return
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		log.Printf("pprof disabled: PPROF_ADDR must be loopback-only")
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	go func() {
		log.Printf("pprof listening on %s (loopback only)", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("pprof server stopped: %v", err)
		}
	}()
}
