// Minimal GraphQL executor for POST /api/graphql.
// Supports the documents the suite sends: viewer/repository/node,
// issues/pullRequests connections with cursor pagination, mutations
// (createIssue, closeIssue, createPullRequest, mergePullRequest,
// addComment), __type introspection, and batched arrays.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- lexer

type gtok struct {
	kind byte // 'n' name, 's' string, 'i' int, 'f' float, 'p' punct, 'v' variable
	s    string
	f    float64
}

func glex(src string) []gtok {
	var toks []gtok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '!' || c == '=' || c == '@':
			toks = append(toks, gtok{kind: 'p', s: string(c)})
			i++
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gtok{kind: 'p', s: "..."})
			i += 3
		case c == '$':
			j := i + 1
			for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			toks = append(toks, gtok{kind: 'v', s: src[i+1 : j]})
			i = j
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					case 'u':
						if j+4 < len(src) {
							if n, err := strconv.ParseUint(src[j+1:j+5], 16, 32); err == nil {
								sb.WriteRune(rune(n))
							}
							j += 4
						}
					default:
						sb.WriteByte(src[j])
					}
					j++
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			toks = append(toks, gtok{kind: 's', s: sb.String()})
			i = j + 1
		case c == '-' || c >= '0' && c <= '9':
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			num := src[i:j]
			f, _ := strconv.ParseFloat(num, 64)
			if isFloat {
				toks = append(toks, gtok{kind: 'f', s: num, f: f})
			} else {
				toks = append(toks, gtok{kind: 'i', s: num, f: f})
			}
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			toks = append(toks, gtok{kind: 'n', s: src[i:j]})
			i = j
		default:
			i++
		}
	}
	return toks
}

// ---------------------------------------------------------------- parser

type gvarRef struct{ name string }

type gfield struct {
	name   string
	alias  string
	args   map[string]jval
	sel    []*gfield
	fragOn string // non-empty for inline fragments (name == "...")
}

type gop struct {
	kind string // "query" | "mutation"
	sel  []*gfield
}

type gparser struct {
	toks []gtok
	pos  int
}

func (p *gparser) peek() (gtok, bool) {
	if p.pos >= len(p.toks) {
		return gtok{}, false
	}
	return p.toks[p.pos], true
}

func (p *gparser) next() (gtok, bool) {
	t, ok := p.peek()
	if ok {
		p.pos++
	}
	return t, ok
}

func (p *gparser) isPunct(s string) bool {
	t, ok := p.peek()
	return ok && t.kind == 'p' && t.s == s
}

func (p *gparser) skipPunct(s string) bool {
	if p.isPunct(s) {
		p.pos++
		return true
	}
	return false
}

// skipBalanced skips a balanced ( ... ) group, used for variable
// definitions and directives (their contents are not needed).
func (p *gparser) skipBalanced() {
	if !p.skipPunct("(") {
		return
	}
	depth := 1
	for p.pos < len(p.toks) && depth > 0 {
		t := p.toks[p.pos]
		p.pos++
		if t.kind == 'p' && t.s == "(" {
			depth++
		} else if t.kind == 'p' && t.s == ")" {
			depth--
		}
	}
}

func (p *gparser) skipDirectives() {
	for p.isPunct("@") {
		p.pos++
		p.next() // directive name
		if p.isPunct("(") {
			p.skipBalanced()
		}
	}
}

func (p *gparser) parseValue() (jval, string) {
	t, ok := p.next()
	if !ok {
		return nil, "unexpected end of query"
	}
	switch t.kind {
	case 'v':
		return gvarRef{t.s}, ""
	case 's':
		return t.s, ""
	case 'i', 'f':
		return t.f, ""
	case 'n':
		switch t.s {
		case "true":
			return true, ""
		case "false":
			return false, ""
		case "null":
			return nil, ""
		default:
			return t.s, "" // enum literal
		}
	case 'p':
		switch t.s {
		case "[":
			var arr []jval
			for !p.isPunct("]") {
				if _, ok := p.peek(); !ok {
					return nil, "unterminated list"
				}
				v, err := p.parseValue()
				if err != "" {
					return nil, err
				}
				arr = append(arr, v)
			}
			p.pos++
			return arr, ""
		case "{":
			m := map[string]jval{}
			for !p.isPunct("}") {
				if _, ok := p.peek(); !ok {
					return nil, "unterminated object"
				}
				kt, ok := p.next()
				if !ok || kt.kind != 'n' {
					return nil, "expected object key"
				}
				if !p.skipPunct(":") {
					return nil, "expected ':'"
				}
				v, err := p.parseValue()
				if err != "" {
					return nil, err
				}
				m[kt.s] = v
			}
			p.pos++
			return m, ""
		}
	}
	return nil, "unexpected token in value"
}

func (p *gparser) parseArgs() (map[string]jval, string) {
	m := map[string]jval{}
	if !p.skipPunct("(") {
		return m, ""
	}
	for !p.isPunct(")") {
		if _, ok := p.peek(); !ok {
			return nil, "unterminated arguments"
		}
		kt, ok := p.next()
		if !ok || kt.kind != 'n' {
			return nil, "expected argument name"
		}
		if !p.skipPunct(":") {
			return nil, "expected ':'"
		}
		v, err := p.parseValue()
		if err != "" {
			return nil, err
		}
		m[kt.s] = v
	}
	p.pos++
	return m, ""
}

func (p *gparser) parseField() (*gfield, string) {
	if p.isPunct("...") {
		p.pos++
		// inline fragment: ... on Type { ... } | named fragment: ...Name
		if t, ok := p.peek(); ok && t.kind == 'n' && t.s == "on" {
			p.pos++
			tn, ok := p.next()
			if !ok || tn.kind != 'n' {
				return nil, "expected type name after 'on'"
			}
			sel, err := p.parseSelectionSet()
			if err != "" {
				return nil, err
			}
			return &gfield{name: "...", fragOn: tn.s, sel: sel}, ""
		}
		// named fragment spread: skip the name (not used by the suite)
		p.next()
		return &gfield{name: "...", fragOn: "\x00skip"}, ""
	}
	nt, ok := p.next()
	if !ok || nt.kind != 'n' {
		return nil, "expected field name"
	}
	f := &gfield{name: nt.s}
	if p.skipPunct(":") {
		at, ok := p.next()
		if !ok || at.kind != 'n' {
			return nil, "expected field name after alias"
		}
		f.alias = f.name
		f.name = at.s
	}
	if p.isPunct("(") {
		args, err := p.parseArgs()
		if err != "" {
			return nil, err
		}
		f.args = args
	}
	p.skipDirectives()
	if p.isPunct("{") {
		sel, err := p.parseSelectionSet()
		if err != "" {
			return nil, err
		}
		f.sel = sel
	}
	return f, ""
}

func (p *gparser) parseSelectionSet() ([]*gfield, string) {
	if !p.skipPunct("{") {
		return nil, "expected '{'"
	}
	var sel []*gfield
	for !p.isPunct("}") {
		if _, ok := p.peek(); !ok {
			return nil, "unterminated selection set"
		}
		f, err := p.parseField()
		if err != "" {
			return nil, err
		}
		sel = append(sel, f)
	}
	p.pos++
	return sel, ""
}

func (p *gparser) parseOperation() (*gop, string) {
	if p.isPunct("{") {
		sel, err := p.parseSelectionSet()
		if err != "" {
			return nil, err
		}
		return &gop{kind: "query", sel: sel}, ""
	}
	t, ok := p.next()
	if !ok || t.kind != 'n' {
		return nil, "expected operation"
	}
	switch t.s {
	case "fragment":
		// fragment Name on Type { ... } — parsed and discarded
		p.next() // name
		p.next() // "on"
		p.next() // type
		_, err := p.parseSelectionSet()
		if err != "" {
			return nil, err
		}
		return &gop{kind: "fragment"}, ""
	case "query", "mutation":
		op := &gop{kind: t.s}
		// optional operation name
		if nt, ok := p.peek(); ok && nt.kind == 'n' {
			p.pos++
		}
		// optional variable definitions
		if p.isPunct("(") {
			p.skipBalanced()
		}
		p.skipDirectives()
		sel, err := p.parseSelectionSet()
		if err != "" {
			return nil, err
		}
		op.sel = sel
		return op, ""
	}
	return nil, "unexpected operation " + t.s
}

func parseGraphQL(src string) ([]*gop, string) {
	p := &gparser{toks: glex(src)}
	var ops []*gop
	for p.pos < len(p.toks) {
		op, err := p.parseOperation()
		if err != "" {
			return nil, err
		}
		if op.kind != "fragment" {
			ops = append(ops, op)
		}
	}
	if len(ops) == 0 {
		return nil, "no operation in document"
	}
	return ops, ""
}

// ---------------------------------------------------------------- execution

type gctx struct {
	s    *server
	vars map[string]jval
	errs []string
}

func (c *gctx) resolveVars(v jval) jval {
	switch t := v.(type) {
	case gvarRef:
		if val, ok := c.vars[t.name]; ok {
			return val
		}
		return nil
	case map[string]jval:
		out := make(map[string]jval, len(t))
		for k, e := range t {
			out[k] = c.resolveVars(e)
		}
		return out
	case []jval:
		out := make([]jval, len(t))
		for i, e := range t {
			out[i] = c.resolveVars(e)
		}
		return out
	}
	return v
}

func (c *gctx) arg(f *gfield, name string) jval {
	if f.args == nil {
		return nil
	}
	v, ok := f.args[name]
	if !ok {
		return nil
	}
	return c.resolveVars(v)
}

func (c *gctx) argStr(f *gfield, name string) string {
	if s, ok := jstr(c.arg(f, name)); ok {
		return s
	}
	return ""
}

func (c *gctx) argInt(f *gfield, name string) int {
	if n, ok := jnum(c.arg(f, name)); ok {
		return int(n)
	}
	return 0
}

func (c *gctx) argObj(f *gfield, name string) map[string]jval {
	if m, ok := jobj(c.arg(f, name)); ok {
		return m
	}
	return nil
}

func (c *gctx) argStrList(f *gfield, name string) []string {
	arr, ok := jarr(c.arg(f, name))
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := jstr(e); ok {
			out = append(out, s)
		}
	}
	return out
}

func (c *gctx) unknown(obj *gobj, f *gfield) jval {
	c.errs = append(c.errs, "Cannot query field \""+f.name+"\" on type \""+obj.kind+"\"")
	return nil
}

type gconn struct {
	items     []*gobj
	hasNext   bool
	endCursor string
}

type gobj struct {
	kind string
	repo *Repo
	is   *issue
	pr   *pull
	cm   *comment
	conn *gconn
	file *fileChange
	// connection edge
	node       *gobj
	edgeCursor string
	// __type
	itype string
	// ref name
	refName string
	// mutation payload child
	child     *gobj
	childName string
	edgeChild *gobj // commentEdge node
}

func encodeCursor(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + itoa(n)))
}

func decodeCursor(cur string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(cur)
	if err != nil {
		return 0, false
	}
	s := strings.TrimPrefix(string(raw), "cursor:")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (c *gctx) execSel(sel []*gfield, obj *gobj) map[string]jval {
	out := map[string]jval{}
	for _, f := range sel {
		if f.name == "..." {
			if f.fragOn == "\x00skip" {
				continue
			}
			if f.fragOn == "" || f.fragOn == obj.kind {
				for k, v := range c.execSel(f.sel, obj) {
					out[k] = v
				}
			}
			continue
		}
		key := f.alias
		if key == "" {
			key = f.name
		}
		out[key] = c.execField(obj, f)
	}
	return out
}

func (c *gctx) execField(obj *gobj, f *gfield) jval {
	switch obj.kind {
	case "Query":
		return c.queryField(f)
	case "Mutation":
		return c.mutationField(f)
	case "User":
		return c.userField(obj, f)
	case "Repository":
		return c.repoField(obj, f)
	case "Issue":
		return c.issueField(obj, f)
	case "PullRequest":
		return c.pullField(obj, f)
	case "Comment":
		return c.commentField(obj, f)
	case "File":
		switch f.name {
		case "path":
			return obj.file.path
		case "__typename":
			return "File"
		}
		return c.unknown(obj, f)
	case "TypeField":
		switch f.name {
		case "name":
			return obj.refName
		case "__typename":
			return "__Field"
		}
		return c.unknown(obj, f)
	case "EnumValue":
		switch f.name {
		case "name":
			return obj.refName
		case "__typename":
			return "__EnumValue"
		}
		return c.unknown(obj, f)
	case "Ref":
		if f.name == "name" {
			return obj.refName
		}
		if f.name == "__typename" {
			return "Ref"
		}
		return c.unknown(obj, f)
	case "IssueConnection", "PullRequestConnection", "FileConnection", "CommentConnection":
		return c.connField(obj, f)
	case "IssueEdge", "PullRequestEdge", "FileEdge", "CommentEdge":
		switch f.name {
		case "node":
			return c.execSel(f.sel, obj.node)
		case "cursor":
			return obj.edgeCursor
		case "__typename":
			return obj.kind
		}
		return c.unknown(obj, f)
	case "PageInfo":
		switch f.name {
		case "hasNextPage":
			return obj.conn.hasNext
		case "hasPreviousPage":
			return false
		case "endCursor":
			if obj.conn.endCursor == "" {
				return nil
			}
			return obj.conn.endCursor
		case "startCursor":
			if len(obj.conn.items) == 0 {
				return nil
			}
			return obj.conn.items[0].edgeCursor
		case "__typename":
			return "PageInfo"
		}
		return c.unknown(obj, f)
	case "Type":
		return c.typeField(obj, f)
	case "Schema":
		switch f.name {
		case "queryType":
			return map[string]jval{"name": "Query"}
		case "mutationType":
			return map[string]jval{"name": "Mutation"}
		case "types":
			return []jval{}
		case "__typename":
			return "__Schema"
		}
		return c.unknown(obj, f)
	case "CreateIssuePayload", "CloseIssuePayload", "CreatePullRequestPayload", "MergePullRequestPayload", "AddCommentPayload":
		return c.payloadField(obj, f)
	}
	return c.unknown(obj, f)
}

// ---- root ----

func (c *gctx) queryField(f *gfield) jval {
	switch f.name {
	case "viewer":
		return c.execSel(f.sel, &gobj{kind: "User"})
	case "repository":
		owner := c.argStr(f, "owner")
		name := c.argStr(f, "name")
		r := c.s.store.lookup(owner, name)
		if r == nil {
			return nil
		}
		return c.execSel(f.sel, &gobj{kind: "Repository", repo: r})
	case "node":
		gid := c.argStr(f, "id")
		kind, parts, ok := decodeGID(gid)
		if !ok {
			return nil
		}
		obj := c.nodeObject(kind, parts)
		if obj == nil {
			return nil
		}
		return c.execSel(f.sel, obj)
	case "__type":
		name := c.argStr(f, "name")
		return c.execSel(f.sel, &gobj{kind: "Type", itype: name})
	case "__schema":
		return c.execSel(f.sel, &gobj{kind: "Schema"})
	case "__typename":
		return "Query"
	}
	return c.unknown(&gobj{kind: "Query"}, f)
}

func (c *gctx) nodeObject(kind string, parts []string) *gobj {
	st := c.s.store
	switch kind {
	case "Repository":
		if len(parts) < 1 {
			return nil
		}
		id, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil
		}
		for _, r := range st.repoList {
			if r.id == id {
				return &gobj{kind: "Repository", repo: r}
			}
		}
	case "Issue":
		if len(parts) < 2 {
			return nil
		}
		r := c.repoByID(parts[0])
		if r == nil {
			return nil
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil
		}
		is := c.s.findIssue(r, n)
		if is == nil {
			return nil
		}
		return &gobj{kind: "Issue", repo: r, is: is}
	case "PullRequest":
		if len(parts) < 2 {
			return nil
		}
		r := c.repoByID(parts[0])
		if r == nil {
			return nil
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil
		}
		pr := c.s.findPull(r, n)
		if pr == nil {
			return nil
		}
		return &gobj{kind: "PullRequest", repo: r, pr: pr}
	case "User":
		return &gobj{kind: "User"}
	}
	return nil
}

func (c *gctx) repoByID(idStr string) *Repo {
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return nil
	}
	for _, r := range c.s.store.repoList {
		if r.id == id {
			return r
		}
	}
	return nil
}

// ---- mutations ----

func (c *gctx) mutationField(f *gfield) jval {
	input := c.argObj(f, "input")
	switch f.name {
	case "createIssue":
		r := c.repoByID(objStr(input, "repositoryId"))
		if r == nil {
			// repositoryId is a global id; decode it
			if kind, parts, ok := decodeGID(objStr(input, "repositoryId")); ok && kind == "Repository" && len(parts) > 0 {
				r = c.repoByID(parts[0])
			}
		}
		if r == nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		is, _ := c.s.createIssue(r, objStr(input, "title"), objStr(input, "body"), nil)
		if is == nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		return c.execSel(f.sel, &gobj{kind: "CreateIssuePayload", child: &gobj{kind: "Issue", repo: r, is: is}, childName: "issue"})
	case "closeIssue":
		gid := objStr(input, "issueId")
		kind, parts, ok := decodeGID(gid)
		if !ok || kind != "Issue" || len(parts) < 2 {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		r := c.repoByID(parts[0])
		if r == nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		is := c.s.findIssue(r, n)
		if is == nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		is.state = "closed"
		is.cached = nil
		return c.execSel(f.sel, &gobj{kind: "CloseIssuePayload", child: &gobj{kind: "Issue", repo: r, is: is}, childName: "issue"})
	case "createPullRequest":
		r := c.repoByID(objStr(input, "repositoryId"))
		if r == nil {
			if kind, parts, ok := decodeGID(objStr(input, "repositoryId")); ok && kind == "Repository" && len(parts) > 0 {
				r = c.repoByID(parts[0])
			}
		}
		if r == nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		pr, _ := c.s.createPull(r, objStr(input, "title"), objStr(input, "body"), objStr(input, "headRefName"), objStr(input, "baseRefName"))
		if pr == nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		return c.execSel(f.sel, &gobj{kind: "CreatePullRequestPayload", child: &gobj{kind: "PullRequest", repo: r, pr: pr}, childName: "pullRequest"})
	case "mergePullRequest":
		gid := objStr(input, "pullRequestId")
		kind, parts, ok := decodeGID(gid)
		if !ok || kind != "PullRequest" || len(parts) < 2 {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		r := c.repoByID(parts[0])
		if r == nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		pr := c.s.findPull(r, n)
		if pr == nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		_, status, _ := c.s.mergePull(r, pr)
		if status != 0 {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		return c.execSel(f.sel, &gobj{kind: "MergePullRequestPayload", child: &gobj{kind: "PullRequest", repo: r, pr: pr}, childName: "pullRequest"})
	case "addComment":
		gid := objStr(input, "subjectId")
		kind, parts, ok := decodeGID(gid)
		if !ok || kind != "Issue" || len(parts) < 2 {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		r := c.repoByID(parts[0])
		if r == nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		is := c.s.findIssue(r, n)
		if is == nil {
			return c.unknown(&gobj{kind: "Mutation"}, f)
		}
		st := c.s.store
		cm := &comment{id: st.nextCommentID, body: objStr(input, "body"), seq: len(is.comments) + 1}
		cm.gid = commentGID(r.id, is.number, cm.id)
		st.nextCommentID++
		is.comments = append(is.comments, cm)
		is.cached = nil
		edge := &gobj{kind: "CommentEdge", node: &gobj{kind: "Comment", repo: r, is: is, cm: cm}, edgeCursor: encodeCursor(cm.id)}
		return c.execSel(f.sel, &gobj{kind: "AddCommentPayload", edgeChild: edge})
	case "__typename":
		return "Mutation"
	}
	return c.unknown(&gobj{kind: "Mutation"}, f)
}

func (c *gctx) payloadField(obj *gobj, f *gfield) jval {
	if obj.edgeChild != nil && f.name == "commentEdge" {
		return c.execSel(f.sel, obj.edgeChild)
	}
	if obj.child != nil && f.name == obj.childName {
		return c.execSel(f.sel, obj.child)
	}
	if f.name == "__typename" {
		return obj.kind
	}
	return c.unknown(obj, f)
}

// ---- object fields ----

func (c *gctx) userField(obj *gobj, f *gfield) jval {
	v := c.s.store.scen.viewer
	switch f.name {
	case "login":
		return v.login
	case "id":
		return float64(v.id)
	case "name":
		return v.name
	case "avatarUrl":
		return ""
	case "url":
		return "https://example.invalid/" + v.login
	case "__typename":
		return "User"
	}
	return c.unknown(obj, f)
}

func (c *gctx) repoField(obj *gobj, f *gfield) jval {
	r := obj.repo
	switch f.name {
	case "id":
		return repoGID(r.id)
	case "databaseId":
		return float64(r.id)
	case "name":
		return r.name
	case "nameWithOwner":
		return r.owner + "/" + r.name
	case "owner":
		return c.execSel(f.sel, &gobj{kind: "User"})
	case "defaultBranchRef":
		return c.execSel(f.sel, &gobj{kind: "Ref", refName: r.defaultBranch})
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "description":
		if r.description == "" {
			return nil
		}
		return r.description
	case "url":
		return "https://example.invalid/" + r.owner + "/" + r.name
	case "createdAt":
		return fixedISO
	case "issue":
		is := c.s.findIssue(r, c.argInt(f, "number"))
		if is == nil {
			return nil
		}
		return c.execSel(f.sel, &gobj{kind: "Issue", repo: r, is: is})
	case "issues":
		return c.execSel(f.sel, c.issueConn(r, f))
	case "pullRequests":
		return c.execSel(f.sel, c.pullConn(r, f))
	case "pullRequest":
		pr := c.s.findPull(r, c.argInt(f, "number"))
		if pr == nil {
			return nil
		}
		return c.execSel(f.sel, &gobj{kind: "PullRequest", repo: r, pr: pr})
	case "__typename":
		return "Repository"
	}
	return c.unknown(obj, f)
}

func (c *gctx) issueField(obj *gobj, f *gfield) jval {
	is := obj.is
	switch f.name {
	case "id":
		return is.gid
	case "number":
		return float64(is.number)
	case "title":
		return is.title
	case "state":
		if is.state == "closed" {
			return "CLOSED"
		}
		return "OPEN"
	case "body":
		return is.body
	case "repository":
		return c.execSel(f.sel, &gobj{kind: "Repository", repo: obj.repo})
	case "author":
		return c.execSel(f.sel, &gobj{kind: "User"})
	case "comments":
		return c.execSel(f.sel, c.commentConn(obj.repo, is, f))
	case "__typename":
		return "Issue"
	}
	return c.unknown(obj, f)
}

func (c *gctx) pullField(obj *gobj, f *gfield) jval {
	pr := obj.pr
	switch f.name {
	case "id":
		return pr.gid
	case "number":
		return float64(pr.number)
	case "title":
		return pr.title
	case "body":
		return pr.body
	case "state":
		if pr.state == "closed" {
			return "CLOSED"
		}
		return "OPEN"
	case "merged":
		return pr.merged
	case "mergeable":
		return c.s.prMergeable(obj.repo, pr)
	case "headRefName":
		return pr.head
	case "baseRefName":
		return pr.base
	case "headRef":
		return c.execSel(f.sel, &gobj{kind: "Ref", refName: pr.head})
	case "baseRef":
		return c.execSel(f.sel, &gobj{kind: "Ref", refName: pr.base})
	case "files":
		return c.execSel(f.sel, c.fileConn(obj.repo, pr, f))
	case "repository":
		return c.execSel(f.sel, &gobj{kind: "Repository", repo: obj.repo})
	case "__typename":
		return "PullRequest"
	}
	return c.unknown(obj, f)
}

func (c *gctx) commentField(obj *gobj, f *gfield) jval {
	switch f.name {
	case "id":
		return obj.cm.gid
	case "body":
		return obj.cm.body
	case "author":
		return c.execSel(f.sel, &gobj{kind: "User"})
	case "__typename":
		return "IssueComment"
	}
	return c.unknown(obj, f)
}

// ---- connections ----

func (c *gctx) issueConn(r *Repo, f *gfield) *gobj {
	states := c.argStrList(f, "states")
	dir := "DESC"
	if ob := c.argObj(f, "orderBy"); ob != nil {
		if d := objStr(ob, "direction"); d != "" {
			dir = d
		}
	}
	var filtered []*issue
	for _, is := range r.issues {
		if len(states) > 0 {
			want := "OPEN"
			if is.state == "closed" {
				want = "CLOSED"
			}
			keep := false
			for _, st := range states {
				if st == want {
					keep = true
					break
				}
			}
			if !keep {
				continue
			}
		}
		filtered = append(filtered, is)
	}
	// numbers are creation order; DESC reverses
	if dir == "DESC" {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	start := 0
	if cur := c.argStr(f, "after"); cur != "" {
		if n, ok := decodeCursor(cur); ok {
			for i, is := range filtered {
				if is.number == n {
					start = i + 1
					break
				}
			}
		}
	}
	first := c.argInt(f, "first")
	end := len(filtered)
	hasNext := false
	if first > 0 && start+first < end {
		end = start + first
		hasNext = true
	}
	page := filtered[start:end]
	var items []*gobj
	for _, is := range page {
		items = append(items, &gobj{kind: "Issue", repo: r, is: is, edgeCursor: encodeCursor(is.number)})
	}
	var endCursor string
	if len(page) > 0 {
		endCursor = encodeCursor(page[len(page)-1].number)
	}
	return &gobj{kind: "IssueConnection", conn: &gconn{items: items, hasNext: hasNext, endCursor: endCursor}}
}

func (c *gctx) pullConn(r *Repo, f *gfield) *gobj {
	first := c.argInt(f, "first")
	start := 0
	if cur := c.argStr(f, "after"); cur != "" {
		if n, ok := decodeCursor(cur); ok {
			for i, pr := range r.pulls {
				if pr.number == n {
					start = i + 1
					break
				}
			}
		}
	}
	end := len(r.pulls)
	hasNext := false
	if first > 0 && start+first < end {
		end = start + first
		hasNext = true
	}
	var items []*gobj
	for _, pr := range r.pulls[start:end] {
		items = append(items, &gobj{kind: "PullRequest", repo: r, pr: pr, edgeCursor: encodeCursor(pr.number)})
	}
	var endCursor string
	if len(items) > 0 {
		endCursor = items[len(items)-1].edgeCursor
	}
	return &gobj{kind: "PullRequestConnection", conn: &gconn{items: items, hasNext: hasNext, endCursor: endCursor}}
}

func (c *gctx) fileConn(r *Repo, pr *pull, f *gfield) *gobj {
	changes := r.pullFiles(pr)
	first := c.argInt(f, "first")
	end := len(changes)
	hasNext := false
	if first > 0 && first < end {
		end = first
		hasNext = true
	}
	var items []*gobj
	for i, fc := range changes[:end] {
		fcCopy := fc
		items = append(items, &gobj{kind: "File", file: &fcCopy, edgeCursor: encodeCursor(i + 1)})
	}
	var endCursor string
	if len(items) > 0 {
		endCursor = items[len(items)-1].edgeCursor
	}
	return &gobj{kind: "FileConnection", conn: &gconn{items: items, hasNext: hasNext, endCursor: endCursor}}
}

func (c *gctx) commentConn(r *Repo, is *issue, f *gfield) *gobj {
	first := c.argInt(f, "first")
	end := len(is.comments)
	hasNext := false
	if first > 0 && first < end {
		end = first
		hasNext = true
	}
	var items []*gobj
	for _, cm := range is.comments[:end] {
		items = append(items, &gobj{kind: "Comment", repo: r, is: is, cm: cm, edgeCursor: encodeCursor(cm.id)})
	}
	var endCursor string
	if len(items) > 0 {
		endCursor = items[len(items)-1].edgeCursor
	}
	return &gobj{kind: "CommentConnection", conn: &gconn{items: items, hasNext: hasNext, endCursor: endCursor}}
}

func (c *gctx) connField(obj *gobj, f *gfield) jval {
	switch f.name {
	case "nodes":
		var out []jval
		for _, item := range obj.conn.items {
			out = append(out, c.execSel(f.sel, item))
		}
		if out == nil {
			out = []jval{}
		}
		return out
	case "edges":
		var out []jval
		edgeKind := strings.TrimSuffix(obj.kind, "Connection") + "Edge"
		for _, item := range obj.conn.items {
			edge := &gobj{kind: edgeKind, node: item, edgeCursor: item.edgeCursor}
			out = append(out, c.execSel(f.sel, edge))
		}
		if out == nil {
			out = []jval{}
		}
		return out
	case "pageInfo":
		return c.execSel(f.sel, &gobj{kind: "PageInfo", conn: obj.conn})
	case "totalCount":
		return float64(len(obj.conn.items))
	case "__typename":
		return obj.kind
	}
	return c.unknown(obj, f)
}

// ---- introspection ----

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"Query":       {"viewer", "repository", "node"},
	"Mutation":    {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":        {"login", "id", "name"},
}

var gqlTypeEnums = map[string][]string{
	"SearchType": {"ISSUE", "REPOSITORY", "USER"},
}

func (c *gctx) typeField(obj *gobj, f *gfield) jval {
	switch f.name {
	case "name":
		return obj.itype
	case "kind":
		if _, ok := gqlTypeEnums[obj.itype]; ok {
			return "ENUM"
		}
		return "OBJECT"
	case "description":
		return nil
	case "fields":
		fields, ok := gqlTypeFields[obj.itype]
		if !ok {
			return nil
		}
		var out []jval
		for _, name := range fields {
			out = append(out, c.execSel(f.sel, &gobj{kind: "TypeField", refName: name}))
		}
		return out
	case "enumValues":
		vals, ok := gqlTypeEnums[obj.itype]
		if !ok {
			return nil
		}
		var out []jval
		for _, name := range vals {
			out = append(out, c.execSel(f.sel, &gobj{kind: "EnumValue", refName: name}))
		}
		return out
	case "__typename":
		return "__Type"
	}
	return c.unknown(obj, f)
}

// ---- entry point ----

func (s *server) graphql(rq *request) *response {
	v, ok := parseJSON(rq.body)
	if !ok {
		var w jw
		w.raw(`{"data":null,"errors":[{"message":"malformed JSON"}]}`)
		return jsonResp(400, w.b)
	}
	if arr, ok := jarr(v); ok {
		var w jw
		w.raw("[")
		for i, el := range arr {
			if i > 0 {
				w.raw(",")
			}
			w.bytes(s.graphqlOne(el))
		}
		w.raw("]")
		return jsonResp(200, w.b)
	}
	return jsonResp(200, s.graphqlOne(v))
}

func (s *server) graphqlOne(doc jval) []byte {
	m, ok := jobj(doc)
	if !ok {
		return []byte(`{"data":null,"errors":[{"message":"expected an object"}]}`)
	}
	query, _ := jstr(m["query"])
	vars := map[string]jval{}
	if vm, ok := jobj(m["variables"]); ok {
		vars = vm
	}
	ops, perr := parseGraphQL(query)
	if perr != "" {
		return []byte(`{"data":null,"errors":[{"message":"` + jsonEscape(perr) + `"}]}`)
	}
	op := ops[0]
	ctx := &gctx{s: s, vars: vars}
	root := &gobj{kind: "Query"}
	if op.kind == "mutation" {
		root.kind = "Mutation"
	}
	data := ctx.execSel(op.sel, root)
	var w jw
	w.raw(`{"data":`)
	writeJVal(&w, data)
	if len(ctx.errs) > 0 {
		w.raw(`,"errors":[`)
		for i, e := range ctx.errs {
			if i > 0 {
				w.raw(",")
			}
			w.raw(`{"message":`)
			w.str(e)
			w.raw(`}`)
		}
		w.raw(`]`)
	}
	w.raw(`}`)
	return w.b
}

// writeJVal serializes a generic JSON value.
func writeJVal(w *jw, v jval) {
	switch t := v.(type) {
	case nil:
		w.null()
	case string:
		w.str(t)
	case bool:
		w.bool(t)
	case float64:
		if t == float64(int64(t)) {
			w.int(int(t))
		} else {
			w.raw(strconv.FormatFloat(t, 'g', -1, 64))
		}
	case int:
		w.int(t)
	case []jval:
		w.raw("[")
		for i, e := range t {
			if i > 0 {
				w.raw(",")
			}
			writeJVal(w, e)
		}
		w.raw("]")
	case map[string]jval:
		w.raw("{")
		first := true
		for k, e := range t {
			if !first {
				w.raw(",")
			}
			first = false
			w.str(k)
			w.raw(":")
			writeJVal(w, e)
		}
		w.raw("}")
	default:
		w.null()
	}
}
