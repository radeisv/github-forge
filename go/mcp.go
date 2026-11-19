// MCP endpoint: POST /mcp, JSON-RPC 2.0 Streamable HTTP.
// Modern first (server/discover, revision 2026-07-28), legacy initialize
// fallback. Tools act on the same store as REST/GraphQL.
package main

import (
	"strconv"
	"strings"
	"sync"
)

var mcpToolsOnce sync.Once
var mcpToolsResult []byte // frozen {"tools":[...]} result bytes

func mcpToolsBytes() []byte {
	mcpToolsOnce.Do(func() {
		mcpToolsResult = []byte(`{"tools":[` +
			`{"name":"create_repo","description":"Create a repository owned by the viewer","inputSchema":{"type":"object","properties":{"name":{"type":"string"},"owner":{"type":"string"},"description":{"type":"string"},"private":{"type":"boolean"}},"required":["name"]}},` +
			`{"name":"get_repo","description":"Get a repository by owner and name","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"}},"required":["owner","repo"]}},` +
			`{"name":"create_issue","description":"Create an issue in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"}},"required":["owner","repo","title"]}},` +
			`{"name":"list_issues","description":"List issues in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"state":{"type":"string"}},"required":["owner","repo"]}},` +
			`{"name":"create_pull_request","description":"Create a pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},"head":{"type":"string"},"base":{"type":"string"}},"required":["owner","repo","title","head","base"]}},` +
			`{"name":"merge_pull_request","description":"Merge a pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"pull_number":{"type":"integer"}},"required":["owner","repo","pull_number"]}}` +
			`]}`)
	})
	return mcpToolsResult
}

var mcpVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// mcp handles POST /mcp. It takes its own store lock, per method (see
// the switch below); the caller holds none.
func (s *server) mcp(rq *request) *response {
	v, ok := parseJSON(rq.body)
	if !ok {
		return jsonResp(400, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`))
	}
	m, ok := jobj(v)
	if !ok {
		return jsonResp(400, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`))
	}
	method, _ := jstr(m["method"])
	id, hasID := m["id"]
	if !hasID {
		// notification: no response (202, empty body)
		return &response{status: 202, contentType: jsonCT, body: []byte{}}
	}
	params, _ := jobj(m["params"])

	// Lock only as much as the method needs. The read-only methods
	// touch nothing in the store; read-only tools take a read lock so
	// concurrent reads are not serialised behind them; only the
	// mutating tools take the write lock.
	switch method {
	case "server/discover", "initialize", "ping", "tools/list":
		// no store access; no lock
	case "tools/call":
		toolName, _ := jstr(params["name"])
		switch strings.ToLower(toolName) {
		case "create_repo", "create_issue", "create_pull_request", "merge_pull_request":
			s.store.mu.Lock()
			defer s.store.mu.Unlock()
		default:
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
		}
	default:
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
	}

	// envelope helpers
	result := func(res []byte) *response {
		var w jw
		w.raw(`{"jsonrpc":"2.0","id":`)
		writeJVal(&w, id)
		w.raw(`,"result":`)
		w.bytes(res)
		w.raw(`}`)
		return jsonResp(200, w.b)
	}
	errResp := func(code int, msg string) *response {
		var w jw
		w.raw(`{"jsonrpc":"2.0","id":`)
		writeJVal(&w, id)
		w.raw(`,"error":{"code":`)
		w.int(code)
		w.raw(`,"message":`)
		w.str(msg)
		w.raw(`}}`)
		return jsonResp(200, w.b)
	}

	switch method {
	case "server/discover":
		var w jw
		w.raw(`{"supportedVersions":["2026-07-28","2025-11-25","2025-06-18","2025-03-26","2024-11-05"],`)
		w.raw(`"serverInfo":{"name":"forge","version":"1.0.0"},`)
		w.raw(`"capabilities":{"tools":{}},`)
		w.raw(`"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"forge","version":"1.0.0"}}}`)
		return result(w.b)
	case "initialize":
		ver := "2025-11-25"
		if pv, ok := jstr(params["protocolVersion"]); ok {
			for _, v := range mcpVersions {
				if v == pv {
					ver = pv
					break
				}
			}
		}
		var w jw
		w.raw(`{"protocolVersion":`)
		w.str(ver)
		w.raw(`,"capabilities":{"tools":{}},"serverInfo":{"name":"forge","version":"1.0.0"}}`)
		return result(w.b)
	case "ping":
		return result([]byte(`{}`))
	case "tools/list":
		return result(mcpToolsBytes())
	case "tools/call":
		return s.mcpCall(id, params, result, errResp)
	default:
		return errResp(-32601, "Method not found: "+method)
	}
}

// mcpCall executes a tools/call request.
func (s *server) mcpCall(id jval, params map[string]jval, result func([]byte) *response, errResp func(int, string) *response) *response {
	name, _ := jstr(params["name"])
	args, _ := jobj(params["arguments"])
	if args == nil {
		args = map[string]jval{}
	}
	// tool result envelope: {"content":[{"type":"text","text":"..."}],
	// "structuredContent": <obj>, "isError": bool}
	toolResult := func(text string, structured jval, isError bool) *response {
		var w jw
		w.raw(`{"content":[{"type":"text","text":`)
		w.str(text)
		w.raw(`}],"structuredContent":`)
		if structured == nil {
			w.null()
		} else {
			writeJVal(&w, structured)
		}
		if isError {
			w.raw(`,"isError":true`)
		}
		w.raw(`}`)
		return result(w.b)
	}
	toolErr := func(msg string) *response {
		return toolResult(msg, map[string]jval{"error": msg}, true)
	}

	owner, repo := mcpOwnerRepo(args)
	st := s.store

	switch strings.ToLower(name) {
	case "create_repo":
		repoName := objStr(args, "name")
		if repoName == "" {
			return toolErr("missing required argument: name")
		}
		if r := st.lookup(st.scen.viewer.login, repoName); r != nil {
			return toolResult(string(repoJSON(st.scen, r)), mustParseJSON(repoJSON(st.scen, r)), false)
		}
		r := st.createRepo(repoName, objStr(args, "description"), objBool(args, "private"))
		if r == nil {
			return toolErr("repository already exists")
		}
		return toolResult(string(repoJSON(st.scen, r)), mustParseJSON(repoJSON(st.scen, r)), false)
	case "get_repo":
		if owner == "" || repo == "" {
			return toolErr("missing required arguments: owner, repo")
		}
		r := st.lookup(owner, repo)
		if r == nil {
			return toolErr("repository not found: " + owner + "/" + repo)
		}
		return toolResult(string(repoJSON(st.scen, r)), mustParseJSON(repoJSON(st.scen, r)), false)
	case "create_issue":
		if owner == "" || repo == "" {
			return toolErr("missing required arguments: owner, repo")
		}
		title := objStr(args, "title")
		if title == "" {
			return toolErr("missing required argument: title")
		}
		r := st.lookup(owner, repo)
		if r == nil {
			return toolErr("repository not found: " + owner + "/" + repo)
		}
		is, _ := s.createIssue(r, title, objStr(args, "body"), nil)
		if is == nil {
			return toolErr("failed to create issue")
		}
		return toolResult(string(issueJSON(st.scen, r, is)), mustParseJSON(issueJSON(st.scen, r, is)), false)
	case "list_issues":
		if owner == "" || repo == "" {
			return toolErr("missing required arguments: owner, repo")
		}
		r := st.lookup(owner, repo)
		if r == nil {
			return toolErr("repository not found: " + owner + "/" + repo)
		}
		state := objStr(args, "state")
		var w jw
		w.raw("[")
		first := true
		for _, is := range r.issues {
			if state == "open" && is.state != "open" {
				continue
			}
			if state == "closed" && is.state != "closed" {
				continue
			}
			if !first {
				w.raw(",")
			}
			first = false
			w.bytes(issueJSON(st.scen, r, is))
		}
		w.raw("]")
		text := string(w.b)
		return toolResult(text, map[string]jval{"issues": mustParseJSON(w.b)}, false)
	case "create_pull_request":
		if owner == "" || repo == "" {
			return toolErr("missing required arguments: owner, repo")
		}
		title := objStr(args, "title")
		head := objStr(args, "head")
		base := objStr(args, "base")
		if title == "" || head == "" || base == "" {
			return toolErr("missing required arguments: title, head, base")
		}
		r := st.lookup(owner, repo)
		if r == nil {
			return toolErr("repository not found: " + owner + "/" + repo)
		}
		pr, msg := s.createPull(r, title, objStr(args, "body"), head, base)
		if pr == nil {
			return toolErr("failed to create pull request: " + msg)
		}
		return toolResult(string(pullJSON(st.scen, r, pr)), mustParseJSON(pullJSON(st.scen, r, pr)), false)
	case "merge_pull_request":
		if owner == "" || repo == "" {
			return toolErr("missing required arguments: owner, repo")
		}
		n, ok := jnum(args["pull_number"])
		if !ok {
			// accept a string number too
			if s, ok := jstr(args["pull_number"]); ok {
				if v, err := strconv.Atoi(s); err == nil {
					n = float64(v)
					ok = true
				}
			}
		}
		if !ok {
			return toolErr("missing required argument: pull_number")
		}
		r := st.lookup(owner, repo)
		if r == nil {
			return toolErr("repository not found: " + owner + "/" + repo)
		}
		pr := s.findPull(r, int(n))
		if pr == nil {
			return toolErr("pull request not found")
		}
		mergeSha, status, msg := s.mergePull(r, pr)
		if status != 0 {
			return toolErr(msg)
		}
		var w jw
		w.raw(`{"merged":true,"sha":`)
		w.str(mergeSha)
		w.raw(`,"message":`)
		w.str(msg)
		w.raw(`}`)
		return toolResult(string(w.b), mustParseJSON(w.b), false)
	default:
		return errResp(-32602, "Unknown tool: "+name)
	}
}

// mcpOwnerRepo resolves owner/repo from tool arguments, accepting
// full_name ("owner/repo") as a fallback.
func mcpOwnerRepo(args map[string]jval) (string, string) {
	owner := objStr(args, "owner")
	repo := objStr(args, "repo")
	if owner == "" || repo == "" {
		if full, ok := jstr(args["full_name"]); ok {
			if i := strings.IndexByte(full, '/'); i > 0 {
				if owner == "" {
					owner = full[:i]
				}
				if repo == "" {
					repo = full[i+1:]
				}
			}
		}
	}
	if repo == "" {
		// some clients send the repo name under "name" or "repository"
		repo = objStr(args, "name")
		if repo == "" {
			repo = objStr(args, "repository")
		}
	}
	return owner, repo
}

func mustParseJSON(b []byte) jval {
	v, _ := parseJSON(b)
	return v
}
