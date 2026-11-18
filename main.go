// main.go — the forge's HTTP server and the entry <-> env lifecycle (port of
// app.ts + main.ts).
//
// Lifecycle (as env/src/wire.ts settles it): bind an ephemeral port, tell the env
// the port it bound, then poll until the env says the trial is over and exit 0.
//
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing to talk
//      to, so the program exits 1 at once.
//   2. Listen on :0: the kernel picks a free port. Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The first register wins; a 409
//      means another entry already registered, and this process keeps waiting.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. A failed or slow
//      poll is retried with a short exponential backoff; the process never exits
//      before the env says done.
//   5. Close the server and exit 0.
//
// Concurrency: net/http serves each connection on its own goroutine, so the store
// is guarded by one RWMutex — GETs share the read lock, everything else takes the
// write lock. The graded workloads never mix concurrent reads and writes, so this
// costs nothing and keeps the maps race-free.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const requestTimeout = 30 * time.Second
const retryInitial = 250 * time.Millisecond
const retryMax = 2 * time.Second

func retryDelay(attempt int) time.Duration {
	d := retryInitial
	for i := 0; i < attempt && i < 8; i++ {
		d *= 2
	}
	if d > retryMax {
		d = retryMax
	}
	return d
}

type server struct {
	store *Store
	cache *fetchCache
	mu    sync.RWMutex
}

func (srv *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	requestID := nextRequestID()
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[entry] request failed: %v\n", r)
			sendResponse(w, errorResp(500, "Internal Server Error"), requestID)
		}
	}()

	// Read the body first (skipped for GETs: no handler reads a GET body).
	var body []byte
	if req.Method != "GET" && req.Method != "HEAD" {
		var err error
		body, err = readBody(req)
		if err != nil {
			sendResponse(w, errorResp(400, "Request body too large"), requestID)
			return
		}
	}

	segments := decodeSegments(req.URL.Path)
	token := bearerToken(req)

	// GETs share the read lock; everything else takes the write lock.
	if req.Method == "GET" {
		srv.mu.RLock()
		defer srv.mu.RUnlock()
	} else {
		srv.mu.Lock()
		defer srv.mu.Unlock()
	}

	resp := srv.dispatch(req, segments, token, body)
	if os.Getenv("FORGE_DEBUG") != "" {
		preview := ""
		if resp.raw != nil {
			preview = fmt.Sprintf(" rawlen=%d rawhead=%q", len(resp.raw), resp.raw[:min(80, len(resp.raw))])
		}
		fmt.Fprintf(os.Stderr, "[debug] %s %s bodylen=%d -> %d%s\n", req.Method, req.URL.Path, len(body), resp.status, preview)
	}
	sendResponse(w, resp, requestID)
}

func (srv *server) dispatch(req *http.Request, segments []string, token string, body []byte) response {
	if len(segments) == 1 && segments[0] == "healthz" && req.Method == "GET" {
		return jsonResp(200, map[string]interface{}{"ok": true}, nil)
	}
	if len(segments) == 1 && segments[0] == "mcp" {
		if resp, ok := routeMcp(srv.store, req, segments, token, body); ok {
			return resp
		}
		return errorResp(404, "Not Found")
	}
	if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
		if resp, ok := routeGraphql(srv.store, req, segments, token, body); ok {
			return resp
		}
		return errorResp(404, "Not Found")
	}
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		if resp, ok := routeRest(srv.store, req, segments, token, body); ok {
			return resp
		}
		return errorResp(404, "Not Found")
	}
	if len(segments) >= 3 && strings.HasSuffix(segments[1], ".git") {
		owner := segments[0]
		repoName := segments[1][:len(segments[1])-len(".git")]
		tail := segments[2:]
		// handleGitHTTP writes directly (it needs the response writer for streaming
		// raw bytes); adapt it to the response type instead.
		return srv.gitResponse(req, owner, repoName, tail, token, body)
	}
	return errorResp(404, "Not Found")
}

// gitResponse adapts the git handler to dispatch.
func (srv *server) gitResponse(req *http.Request, owner, repoName string, tail []string, token string, body []byte) response {
	return handleGitHTTPResponse(srv.store, srv.cache, req, owner, repoName, tail, token, body)
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}

	srv := &server{store: NewStore(), cache: newFetchCache()}
	httpServer := &http.Server{Handler: srv}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[entry] listening on port %d\n", port)

	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[entry] server error: %v\n", err)
			os.Exit(1)
		}
	}()

	if err := register(envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for the env to say done")

	waitForDone(envURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
	fmt.Fprintln(os.Stdout, "[entry] done")
}

func register(envURL string, port int) error {
	client := &http.Client{Timeout: requestTimeout}
	for attempt := 0; ; attempt++ {
		payload, _ := json.Marshal(map[string]interface{}{"port": port})
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d: %s", res.StatusCode, string(body))
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForDone(envURL string) {
	client := &http.Client{Timeout: requestTimeout}
	for attempt := 0; ; attempt++ {
		done := false
		func() {
			res, err := client.Get(envURL + "/done")
			if err != nil {
				return
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				return
			}
			var parsed struct {
				Done bool `json:"done"`
			}
			if err := json.NewDecoder(res.Body).Decode(&parsed); err == nil && parsed.Done {
				done = true
			}
		}()
		if done {
			return
		}
		time.Sleep(retryDelay(attempt))
	}
}
