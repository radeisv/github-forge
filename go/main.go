// forge: a GitHub-compatible git forge in one static binary.
// In-memory store, hand-rolled HTTP/1.1, git smart HTTP, REST v3,
// GraphQL and MCP — all from the same store. No wall clock in any
// stored or returned state (fixed timestamps only).
package main

import (
	"bytes"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	scen := buildScenario()
	store := newStore(token, scen)
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	os.Stdout.WriteString("FORGE_PORT=" + itoa(port) + "\n")
	srv := &server{store: store, ln: ln}
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		ln.Close()
		os.Exit(0)
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go srv.serveConn(conn)
	}
}

// dispatch routes one request. It owns the store lock: read lock for
// read-only routes, write lock for anything that can mutate.
func (s *server) dispatch(rq *request) *response {
	if rq.method == "GET" && rq.path == "/healthz" {
		return jsonResp(200, []byte(`{"ok":true}`))
	}
	if s.store.token == "" || rq.bearer() != s.store.token {
		return errJSON(401, "Bad credentials")
	}
	segs := rq.segments
	// REST v3
	if len(segs) >= 2 && segs[0] == "api" && segs[1] == "v3" {
		if rq.method == "GET" {
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
		} else {
			s.store.mu.Lock()
			defer s.store.mu.Unlock()
		}
		return s.restDispatch(rq)
	}
	// GraphQL
	if len(segs) == 2 && segs[0] == "api" && segs[1] == "graphql" {
		if rq.method != "POST" {
			return notFoundJSON("")
		}
		// bytes.Contains on the raw body: the old code converted the
		// whole body to a string first, one throwaway copy per query.
		if bytes.Contains(rq.body, []byte("mutation")) {
			s.store.mu.Lock()
			defer s.store.mu.Unlock()
		} else {
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
		}
		return s.graphql(rq)
	}
	// MCP: no lock is taken here. mcp() locks per method — read-only
	// methods take none, read-only tools take a read lock, and only
	// the mutating tools take the write lock — so a tools/list or
	// get_repo no longer serialises behind every other MCP call.
	if len(segs) == 1 && segs[0] == "mcp" {
		if rq.method != "POST" {
			return notFoundJSON("")
		}
		return s.mcp(rq)
	}
	// git smart HTTP: /{owner}/{repo}.git/...
	if len(segs) >= 2 && strings.HasSuffix(segs[1], ".git") {
		owner := segs[0]
		repoName := strings.TrimSuffix(segs[1], ".git")
		if len(segs) == 4 && segs[2] == "info" && segs[3] == "refs" && rq.method == "GET" {
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			service := rq.queryParam("service")
			if service == "" {
				service = "git-upload-pack"
			}
			return s.gitAdvertisement(rq, owner, repoName, service)
		}
		if len(segs) == 3 && rq.method == "POST" {
			switch segs[2] {
			case "git-upload-pack":
				s.store.mu.RLock()
				defer s.store.mu.RUnlock()
				r := s.store.lookup(owner, repoName)
				if r == nil {
					return notFoundJSON("Not Found")
				}
				return s.gitUploadPack(rq, r)
			case "git-receive-pack":
				s.store.mu.Lock()
				defer s.store.mu.Unlock()
				r := s.store.lookup(owner, repoName)
				if r == nil {
					return notFoundJSON("Not Found")
				}
				return s.gitReceivePack(rq, r)
			}
		}
		return notFoundJSON("")
	}
	return notFoundJSON("")
}
