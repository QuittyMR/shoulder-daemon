// Phase 0 spike listener: records every Claude Code HTTP hook request verbatim
// and replies according to a control file that is re-read on every request.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type control struct {
	Mode    string            `json:"mode"` // record | inject | delay | error | empty
	DelayMS int               `json:"delay_ms"`
	Nonce   string            `json:"nonce"`
	Events  map[string]string `json:"events"` // per-event mode override
}

type record struct {
	TS         string            `json:"ts"`
	Path       string            `json:"path"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers"`
	Body       json.RawMessage   `json:"body,omitempty"`
	BodyRaw    string            `json:"body_raw,omitempty"`
	Mode       string            `json:"mode"`
	Response   string            `json:"response"`
	ServiceUS  int64             `json:"service_us"`
	ClientGone bool              `json:"client_gone"`
}

var (
	outDir      = env("GC_OUT", "spikes/captures")
	controlPath = env("GC_CONTROL", "spikes/control.json")
	mu          sync.Mutex
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func loadControl() control {
	c := control{Mode: "record"}
	b, err := os.ReadFile(controlPath)
	if err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.Mode == "" {
		c.Mode = "record"
	}
	return c
}

func append_(file string, v any) {
	mu.Lock()
	defer mu.Unlock()
	_ = os.MkdirAll(outDir, 0o755)
	f, err := os.OpenFile(filepath.Join(outDir, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("append %s: %v", file, err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	_ = enc.Encode(v)
}

func main() {
	addr := env("GC_ADDR", "127.0.0.1:8787")

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		_ = r.Body.Close()

		seg := r.URL.Path
		if i := strings.LastIndex(seg, "/"); i >= 0 {
			seg = seg[i+1:]
		}
		if seg == "" {
			seg = "root"
		}

		c := loadControl()
		mode := c.Mode
		if m, ok := c.Events[seg]; ok && m != "" {
			mode = m
		}

		hdr := map[string]string{}
		for k, v := range r.Header {
			hdr[k] = strings.Join(v, ", ")
		}

		clientGone := false
		if mode == "delay" && c.DelayMS > 0 {
			select {
			case <-time.After(time.Duration(c.DelayMS) * time.Millisecond):
			case <-r.Context().Done():
				clientGone = true
			}
		}
		if r.Context().Err() != nil {
			clientGone = true
		}

		var resp string
		switch mode {
		case "error":
			w.WriteHeader(http.StatusInternalServerError)
			resp = `{"error":"spike"}`
			_, _ = io.WriteString(w, resp)
		case "empty":
			resp = ""
			w.WriteHeader(http.StatusOK)
		case "inject":
			nonce := c.Nonce
			if nonce == "" {
				nonce = "GCNONCE-0000"
			}
			out := map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":     seg,
					"additionalContext": "SHOULDER-DAEMON SPIKE MARKER " + nonce + " — if you can read this, reply with the marker verbatim.",
				},
			}
			b, _ := json.Marshal(out)
			resp = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		default: // record
			resp = "{}"
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, resp)
		}

		rec := record{
			TS:         time.Now().UTC().Format(time.RFC3339Nano),
			Path:       r.URL.Path,
			Method:     r.Method,
			Headers:    hdr,
			Mode:       mode,
			ClientGone: clientGone,
			Response:   resp,
			ServiceUS:  time.Since(start).Microseconds(),
		}
		if json.Valid(body) {
			rec.Body = json.RawMessage(body)
		} else {
			rec.BodyRaw = string(body)
		}
		append_("all.jsonl", rec)
		append_(seg+".jsonl", rec)
		log.Printf("%s %s mode=%s bytes=%d %dus client_gone=%v", r.Method, r.URL.Path, mode, len(body), rec.ServiceUS, clientGone)
	})

	log.Printf("shoulder-daemon spike listener on %s, out=%s control=%s", addr, outDir, controlPath)
	srv := &http.Server{Addr: addr, Handler: mux}
	log.Fatal(srv.ListenAndServe())
}
