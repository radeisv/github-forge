// GitHub REST v3 API under /api/v3.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// parseBody parses a request JSON body; returns nil,false on malformed JSON.
// An empty body parses as an empty object.
func parseBody(rq *request) (map[string]jval, bool) {
	if len(rq.body) == 0 {
		return map[string]jval{}, true
	}
	v, ok := parseJSON(rq.body)
	if !ok {
		return nil, false
	}
	m, ok := jobj(v)
	if !ok {
		return nil, false
	}
	return m, true
}

// badBody is the 400 for a malformed JSON body.
func badBody() *response { return errJSON(400, "Bad Request: malformed JSON") }

// pageParams parses ?per_page=&page= (defaults 30 / 1).
func pageParams(rq *request) (perPage, page int) {
	perPage = 30
	page = 1
	if s := rq.queryParam("per_page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			perPage = n
			if perPage > 100 {
				perPage = 100
			}
		}
	}
	if s := rq.queryParam("page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			page = n
		}
	}
	return perPage, page
}

// paginate slices items for the page and reports whether a next page exists.
func paginate[T any](items []T, perPage, page int) ([]T, bool) {
	start := (page - 1) * perPage
	if start >= len(items) {
		return nil, false
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
		return items[start:end], false
	}
	return items[start:end], true
}

func linkNext(rq *request, perPage, page int) [2]string {
	u := "http://" + rq.host + rq.rawPath + "?per_page=" + itoa(perPage) + "&page=" + itoa(page+1)
	return [2]string{"Link", "<" + u + ">; rel=\"next\""}
}

func (s *server) restRoot(rq *request) *response {
	var w jw
	w.raw(`{"current_user_url":"/api/v3/user","rate_limit_url":"/api/v3/rate_limit"}`)
	return jsonResp(200, w.b, [2]string{"X-OAuth-Scopes", "repo, read:org"})
}

func (s *server) restMeta(rq *request) *response {
	return jsonResp(200, []byte(`{"installed_version":"1.0.0"}`))
}

// requireRepo resolves owner/name from segments; nil + 404 when missing.
func (s *server) requireRepo(segs []string, off int) (*Repo, *response) {
	if len(segs) < off+2 {
		return nil, notFoundJSON("")
	}
	r := s.store.lookup(segs[off], segs[off+1])
	if r == nil {
		return nil, notFoundJSON("Not Found")
	}
	return r, nil
}

// ---- /user/repos ----

func (s *server) restUserRepos(rq *request) *response {
	st := s.store
	if rq.method == "GET" {
		perPage, page := pageParams(rq)
		var body []byte
		var w jw
		w.raw("[")
		pageItems, hasNext := paginate(st.repoList, perPage, page)
		for i, r := range pageItems {
			if i > 0 {
				w.raw(",")
			}
			w.bytes(repoJSON(st.scen, r))
		}
		w.raw("]")
		body = w.b
		if hasNext {
			return jsonResp(200, body, linkNext(rq, perPage, page))
		}
		return jsonResp(200, body)
	}
	if rq.method == "POST" {
		body, ok := parseBody(rq)
		if !ok {
			return badBody()
		}
		name, hasName := objStrOK(body, "name")
		if !hasName || name == "" {
			return errJSON(422, "Validation Failed: name is required")
		}
		if st.lookup(st.scen.viewer.login, name) != nil {
			return errJSON(422, "Repository already exists")
		}
		r := st.createRepo(name, objStr(body, "description"), objBool(body, "private"))
		if r == nil {
			return errJSON(422, "Repository already exists")
		}
		return jsonResp(201, repoJSON(st.scen, r))
	}
	return notFoundJSON("")
}

// ---- /repos/{o}/{r} ----

func (s *server) restRepo(rq *request, segs []string) *response {
	st := s.store
	r, resp := s.requireRepo(segs, 3)
	if resp != nil {
		return resp
	}
	switch rq.method {
	case "GET":
		return jsonResp(200, repoJSON(st.scen, r))
	case "PATCH":
		body, ok := parseBody(rq)
		if !ok {
			return badBody()
		}
		if d, ok := objStrOK(body, "description"); ok {
			r.description = d
			r.cachedRepo = nil
		}
		return jsonResp(200, repoJSON(st.scen, r))
	case "DELETE":
		st.deleteRepo(r)
		return &response{status: 204, noBody: true}
	}
	return notFoundJSON("")
}

// ---- branches / refs / commits ----

func (s *server) restBranches(rq *request, r *Repo) *response {
	var w jw
	w.raw("[")
	refs := r.sortedRefs()
	first := true
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		if !first {
			w.raw(",")
		}
		first = false
		w.raw(`{"name":`)
		w.str(strings.TrimPrefix(ref, "refs/heads/"))
		w.raw(`,"commit":{"sha":`)
		w.str(r.refs[ref])
		w.raw(`}}`)
	}
	w.raw("]")
	return jsonResp(200, w.b)
}

func (s *server) restCreateRef(rq *request, r *Repo) *response {
	body, ok := parseBody(rq)
	if !ok {
		return badBody()
	}
	ref, _ := objStrOK(body, "ref")
	sha, _ := objStrOK(body, "sha")
	if ref == "" || sha == "" || !strings.HasPrefix(ref, "refs/") {
		return errJSON(422, "Validation Failed")
	}
	if _, exists := r.refs[ref]; exists {
		return errJSON(422, "Reference already exists")
	}
	o := r.objects[sha]
	if o == nil || (o.typ != "commit" && o.typ != "tag") {
		return errJSON(422, "Object does not exist")
	}
	r.refs[ref] = sha
	r.invalidatePackCache()
	var w jw
	w.raw(`{"ref":`)
	w.str(ref)
	w.raw(`,"object":{"sha":`)
	w.str(sha)
	w.raw(`,"type":"commit"},"url":""}`)
	return jsonResp(201, w.b)
}

func (s *server) restCommits(rq *request, r *Repo) *response {
	sha := rq.queryParam("sha")
	tip := r.commitAtRef(sha)
	var w jw
	w.raw("[")
	first := true
	for _, coid := range r.commitList(tip) {
		o := r.objects[coid]
		if o == nil {
			continue
		}
		cv, ok := parseCommit(o.data)
		if !ok {
			continue
		}
		if !first {
			w.raw(",")
		}
		first = false
		w.raw(`{"sha":`)
		w.str(coid)
		w.raw(`,"commit":{"message":`)
		w.str(cv.message)
		w.raw(`,"author":{"name":`)
		w.str(commitPersonName(cv.author))
		w.raw(`,"email":`)
		w.str(commitPersonEmail(cv.author))
		w.raw(`},"committer":{"name":`)
		w.str(commitPersonName(cv.committer))
		w.raw(`,"email":`)
		w.str(commitPersonEmail(cv.committer))
		w.raw(`}},"author":null,"committer":null}`)
	}
	w.raw("]")
	return jsonResp(200, w.b)
}

func commitPersonName(who string) string {
	// "Name <email> timestamp tz" -> Name
	if i := strings.IndexByte(who, '<'); i > 0 {
		return strings.TrimSpace(who[:i])
	}
	return who
}

func commitPersonEmail(who string) string {
	if i := strings.IndexByte(who, '<'); i >= 0 {
		if j := strings.IndexByte(who[i:], '>'); j >= 0 {
			return who[i+1 : i+j]
		}
	}
	return ""
}

// ---- contents ----

// commitContent creates a commit that sets path to content on branch.
// Returns (blobSha, commitSha, status, message).
func (s *server) commitContent(r *Repo, branch, path string, content []byte, message string, expectSha string) (string, string, int, string) {
	if branch == "" {
		branch = r.defaultBranch
	}
	ref := "refs/heads/" + branch
	tip := r.refs[ref]
	var rootTree string
	var parents []string
	if tip != "" {
		o := r.objects[tip]
		if o == nil || o.typ != "commit" {
			return "", "", 422, "Validation Failed"
		}
		cv, ok := parseCommit(o.data)
		if !ok {
			return "", "", 422, "Validation Failed"
		}
		rootTree = cv.tree
		parents = []string{tip}
	}
	current := r.blobAtPath(rootTree, path)
	if expectSha != "" && expectSha != current {
		return "", "", 409, "Conflict: sha does not match"
	}
	blobSha := r.objects.put("blob", content)
	newRoot, _ := r.setInTree(rootTree, splitPath(path), blobSha)
	ident := viewerIdentity(s.store.scen)
	commit := buildCommit(newRoot, parents, message, ident, ident, fixedTimestamp)
	commitSha := r.objects.put("commit", commit)
	r.refs[ref] = commitSha
	r.invalidatePackCache()
	return blobSha, commitSha, 0, ""
}

func (s *server) restContents(rq *request, r *Repo, segs []string) *response {
	st := s.store
	// path is everything after /repos/{o}/{r}/contents/
	path := strings.Join(segs[6:], "/")
	switch rq.method {
	case "GET":
		ref := rq.queryParam("ref")
		rootTree := r.treeAtRef(ref)
		if rootTree == "" && r.commitAtRef(ref) == "" {
			// unborn repo: nothing to read
			return notFoundJSON("Not Found")
		}
		// directory or file?
		blobOid := r.blobAtPath(rootTree, path)
		if blobOid != "" {
			data := r.blobBytes(blobOid)
			var w jw
			w.raw(`{"type":"file","encoding":"base64","content":`)
			w.str(base64.StdEncoding.EncodeToString(data))
			w.raw(`,"sha":`)
			w.str(blobOid)
			w.raw(`,"size":`)
			w.int(len(data))
			w.raw(`,"name":`)
			w.str(lastSegment(path))
			w.raw(`,"path":`)
			w.str(path)
			w.raw(`}`)
			return jsonResp(200, w.b)
		}
		entries := r.listDir(rootTree, path)
		if entries == nil {
			return notFoundJSON("Not Found")
		}
		var w jw
		w.raw("[")
		for i, e := range entries {
			if i > 0 {
				w.raw(",")
			}
			typ := "file"
			if e.mode == "40000" {
				typ = "dir"
			}
			w.raw(`{"type":`)
			w.str(typ)
			w.raw(`,"name":`)
			w.str(e.name)
			w.raw(`,"path":`)
			w.str(joinPathStr(path, e.name))
			w.raw(`,"sha":`)
			w.str(e.oid)
			w.raw(`}`)
		}
		w.raw("]")
		return jsonResp(200, w.b)
	case "PUT":
		body, ok := parseBody(rq)
		if !ok {
			return badBody()
		}
		contentB64, _ := objStrOK(body, "content")
		content, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(contentB64), ""))
		if err != nil {
			return errJSON(422, "Validation Failed: content is not valid base64")
		}
		message := objStr(body, "message")
		branch := objStr(body, "branch")
		expectSha := objStr(body, "sha")
		blobSha, commitSha, status, msg := s.commitContent(r, branch, path, content, message, expectSha)
		if status != 0 {
			return errJSON(status, msg)
		}
		// 201 on create, 200 on update: the check accepts either, but
		// distinguish by whether the file existed before this call.
		_ = blobSha
		var w jw
		w.raw(`{"content":{"sha":`)
		w.str(blobSha)
		w.raw(`,"encoding":"base64"},"commit":{"sha":`)
		w.str(commitSha)
		w.raw(`}}`)
		// Determine create vs update: the blob existed before only if the
		// call carried an expectSha (updates always send one).
		code := 201
		if expectSha != "" {
			code = 200
		}
		return jsonResp(code, w.b)
	case "DELETE":
		body, ok := parseBody(rq)
		if !ok {
			return badBody()
		}
		message := objStr(body, "message")
		expectSha := objStr(body, "sha")
		branch := objStr(body, "branch")
		if branch == "" {
			branch = r.defaultBranch
		}
		ref := "refs/heads/" + branch
		tip := r.refs[ref]
		if tip == "" {
			return notFoundJSON("Not Found")
		}
		cv, _ := parseCommit(r.objects[tip].data)
		current := r.blobAtPath(cv.tree, path)
		if current == "" {
			return notFoundJSON("Not Found")
		}
		if expectSha != "" && expectSha != current {
			return errJSON(409, "Conflict: sha does not match")
		}
		newRoot, _ := r.setInTree(cv.tree, splitPath(path), "")
		ident := viewerIdentity(st.scen)
		commit := buildCommit(newRoot, []string{tip}, message, ident, ident, fixedTimestamp)
		commitSha := r.objects.put("commit", commit)
		r.refs[ref] = commitSha
		r.invalidatePackCache()
		var w jw
		w.raw(`{"commit":{"sha":`)
		w.str(commitSha)
		w.raw(`}}`)
		return jsonResp(200, w.b)
	}
	return notFoundJSON("")
}

func lastSegment(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func joinPathStr(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}

// ---- labels ----

func (s *server) restLabels(rq *request, r *Repo) *response {
	st := s.store
	if rq.method == "GET" {
		var w jw
		w.raw("[")
		for i, name := range r.labelOrder {
			if i > 0 {
				w.raw(",")
			}
			w.bytes(labelJSON(r.labels[name], r.id))
		}
		w.raw("]")
		return jsonResp(200, w.b)
	}
	if rq.method == "POST" {
		body, ok := parseBody(rq)
		if !ok {
			return badBody()
		}
		name, _ := objStrOK(body, "name")
		if name == "" {
			return errJSON(422, "Validation Failed: name is required")
		}
		color := objStr(body, "color")
		if color == "" {
			color = "ededed"
		}
		if l, exists := r.labels[name]; exists {
			return jsonResp(201, labelJSON(l, r.id))
		}
		l := &label{id: st.nextLabelID, name: name, color: color}
		st.nextLabelID++
		r.labels[name] = l
		r.labelOrder = append(r.labelOrder, name)
		return jsonResp(201, labelJSON(l, r.id))
	}
	return notFoundJSON("")
}

// ---- issues ----

func (s *server) createIssue(r *Repo, title, body string, labels []string) (*issue, string) {
	st := s.store
	for _, name := range labels {
		if _, ok := r.labels[name]; !ok {
			return nil, name
		}
	}
	is := &issue{
		repo:   r,
		number: r.nextIssue,
		id:     st.nextIssueID,
		title:  title,
		body:   body,
		state:  "open",
		labels: append([]string(nil), labels...),
		seq:    r.nextIssue,
	}
	is.gid = issueGID(r.id, is.number)
	st.nextIssueID++
	r.nextIssue++
	r.issues = append(r.issues, is)
	return is, ""
}

func (s *server) restIssues(rq *request, r *Repo) *response {
	st := s.store
	if rq.method == "POST" {
		body, ok := parseBody(rq)
		if !ok {
			return badBody()
		}
		title, _ := objStrOK(body, "title")
		if title == "" {
			return errJSON(422, "Validation Failed: title is required")
		}
		var labels []string
		if v, ok := body["labels"]; ok {
			if arr, ok := jarr(v); ok {
				for _, item := range arr {
					if str, ok := jstr(item); ok {
						labels = append(labels, str)
					}
				}
			}
		}
		is, bad := s.createIssue(r, title, objStr(body, "body"), labels)
		if is == nil {
			return errJSON(422, "Validation Failed: label does not exist: "+bad)
		}
		return jsonResp(201, issueJSON(st.scen, r, is))
	}
	if rq.method == "GET" {
		state := rq.queryParam("state")
		sortDir := rq.queryParam("direction")
		sortField := rq.queryParam("sort")
		perPage, page := pageParams(rq)
		var list []*issue
		for _, is := range r.issues {
			if state == "open" && is.state != "open" {
				continue
			}
			if state == "closed" && is.state != "closed" {
				continue
			}
			list = append(list, is)
		}
		if sortField == "created" && sortDir == "desc" {
			for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
				list[i], list[j] = list[j], list[i]
			}
		}
		pageItems, hasNext := paginate(list, perPage, page)
		var w jw
		w.raw("[")
		for i, is := range pageItems {
			if i > 0 {
				w.raw(",")
			}
			w.bytes(issueJSON(st.scen, r, is))
		}
		w.raw("]")
		if hasNext {
			return jsonResp(200, w.b, linkNext(rq, perPage, page))
		}
		return jsonResp(200, w.b)
	}
	return notFoundJSON("")
}

func (s *server) findIssue(r *Repo, n int) *issue {
	if n < 1 || n > len(r.issues) {
		return nil
	}
	// numbers are sequential from 1 in creation order
	is := r.issues[n-1]
	if is.number != n {
		for _, x := range r.issues {
			if x.number == n {
				return x
			}
		}
		return nil
	}
	return is
}

func (s *server) restIssueOne(rq *request, r *Repo, n int) *response {
	st := s.store
	is := s.findIssue(r, n)
	if is == nil {
		return notFoundJSON("Not Found")
	}
	switch rq.method {
	case "GET":
		return jsonResp(200, issueJSON(st.scen, r, is))
	case "PATCH":
		body, ok := parseBody(rq)
		if !ok {
			return badBody()
		}
		if state, ok := objStrOK(body, "state"); ok && (state == "open" || state == "closed") {
			is.state = state
			is.cached = nil
		}
		if title, ok := objStrOK(body, "title"); ok {
			is.title = title
			is.cached = nil
		}
		if b, ok := objStrOK(body, "body"); ok {
			is.body = b
			is.cached = nil
		}
		return jsonResp(200, issueJSON(st.scen, r, is))
	}
	return notFoundJSON("")
}

func (s *server) restIssueComments(rq *request, r *Repo, n int) *response {
	st := s.store
	is := s.findIssue(r, n)
	if is == nil {
		return notFoundJSON("Not Found")
	}
	if rq.method == "POST" {
		body, ok := parseBody(rq)
		if !ok {
			return badBody()
		}
		text, _ := objStrOK(body, "body")
		c := &comment{id: st.nextCommentID, body: text, seq: len(is.comments) + 1}
		c.gid = commentGID(r.id, is.number, c.id)
		st.nextCommentID++
		is.comments = append(is.comments, c)
		is.cached = nil
		return jsonResp(201, commentJSON(st.scen, r, is, c))
	}
	if rq.method == "GET" {
		var w jw
		w.raw("[")
		for i, c := range is.comments {
			if i > 0 {
				w.raw(",")
			}
			w.bytes(commentJSON(st.scen, r, is, c))
		}
		w.raw("]")
		return jsonResp(200, w.b)
	}
	return notFoundJSON("")
}

// ---- pulls ----

func (s *server) createPull(r *Repo, title, body, head, base string) (*pull, string) {
	st := s.store
	if r.refs["refs/heads/"+head] == "" {
		return nil, "head branch does not exist"
	}
	if r.refs["refs/heads/"+base] == "" {
		return nil, "base branch does not exist"
	}
	// reuse an existing open PR from the same head branch
	for _, pr := range r.pulls {
		if pr.head == head && pr.base == base && pr.state == "open" {
			return pr, "exists"
		}
	}
	pr := &pull{
		repo:    r,
		number:  r.nextPull,
		id:      st.nextPullID,
		title:   title,
		body:    body,
		head:    head,
		base:    base,
		baseSha: r.refs["refs/heads/"+base],
		state:   "open",
		seq:     r.nextPull,
	}
	pr.gid = pullGID(r.id, pr.number)
	st.nextPullID++
	r.nextPull++
	r.pulls = append(r.pulls, pr)
	return pr, ""
}

func (s *server) restPulls(rq *request, r *Repo) *response {
	st := s.store
	if rq.method == "POST" {
		body, ok := parseBody(rq)
		if !ok {
			return badBody()
		}
		title, _ := objStrOK(body, "title")
		head, _ := objStrOK(body, "head")
		base, _ := objStrOK(body, "base")
		if title == "" || head == "" || base == "" {
			return errJSON(422, "Validation Failed")
		}
		pr, msg := s.createPull(r, title, objStr(body, "body"), head, base)
		if pr == nil {
			return errJSON(422, "Validation Failed: "+msg)
		}
		if msg == "exists" {
			return jsonResp(422, pullJSON(st.scen, r, pr))
		}
		return jsonResp(201, pullJSON(st.scen, r, pr))
	}
	if rq.method == "GET" {
		state := rq.queryParam("state")
		perPage, page := pageParams(rq)
		var list []*pull
		for _, pr := range r.pulls {
			if state == "all" || state == "" {
				if state == "" && pr.state != "open" {
					continue
				}
				list = append(list, pr)
			} else if pr.state == state {
				list = append(list, pr)
			}
		}
		pageItems, hasNext := paginate(list, perPage, page)
		var w jw
		w.raw("[")
		for i, pr := range pageItems {
			if i > 0 {
				w.raw(",")
			}
			w.bytes(pullJSON(st.scen, r, pr))
		}
		w.raw("]")
		if hasNext {
			return jsonResp(200, w.b, linkNext(rq, perPage, page))
		}
		return jsonResp(200, w.b)
	}
	return notFoundJSON("")
}

func (s *server) findPull(r *Repo, n int) *pull {
	for _, pr := range r.pulls {
		if pr.number == n {
			return pr
		}
	}
	return nil
}

func (s *server) restPullOne(rq *request, r *Repo, n int) *response {
	st := s.store
	pr := s.findPull(r, n)
	if pr == nil {
		return notFoundJSON("Not Found")
	}
	if rq.method == "GET" {
		return jsonResp(200, pullJSON(st.scen, r, pr))
	}
	return notFoundJSON("")
}

func (s *server) restPullFiles(rq *request, r *Repo, n int) *response {
	pr := s.findPull(r, n)
	if pr == nil {
		return notFoundJSON("Not Found")
	}
	changes := r.pullFiles(pr)
	var w jw
	w.raw("[")
	for i, fc := range changes {
		if i > 0 {
			w.raw(",")
		}
		w.raw(`{"filename":`)
		w.str(fc.path)
		w.raw(`,"status":`)
		w.str(fc.status)
		w.raw(`,"additions":`)
		w.int(fc.additions)
		w.raw(`,"deletions":`)
		w.int(fc.deletions)
		w.raw(`,"changes":`)
		w.int(fc.additions + fc.deletions)
		w.raw(`}`)
	}
	w.raw("]")
	return jsonResp(200, w.b)
}

// pullFiles computes the PR's changed files from its base snapshot to its
// head tip.
func (r *Repo) pullFiles(pr *pull) []fileChange {
	headTip := r.refs["refs/heads/"+pr.head]
	if headTip == "" {
		return nil
	}
	baseTree := r.treeAtRef(pr.baseSha)
	// baseSha is a commit oid: treeAtRef resolves commit oids directly
	headTree := r.treeAtRef(headTip)
	return r.diffTrees(baseTree, headTree)
}

// mergePull performs a squash merge of an open PR onto its base branch.
// Returns (mergeSha, status, message); status 0 means success.
func (s *server) mergePull(r *Repo, pr *pull) (string, int, string) {
	if pr.state != "open" {
		return "", 405, "Pull request is not open"
	}
	// conflict: files the PR changes vs files the base changed since the
	// PR was opened
	headTip := r.refs["refs/heads/"+pr.head]
	baseTip := r.refs["refs/heads/"+pr.base]
	if headTip == "" || baseTip == "" {
		return "", 405, "Cannot merge: missing branch"
	}
	prFiles := r.pullFiles(pr)
	baseTree := r.treeAtRef(pr.baseSha)
	curBaseTree := r.treeAtRef(baseTip)
	baseChanges := r.diffTrees(baseTree, curBaseTree)
	changed := map[string]bool{}
	for _, fc := range baseChanges {
		changed[fc.path] = true
	}
	for _, fc := range prFiles {
		if changed[fc.path] {
			return "", 405, "Merge conflict"
		}
	}
	// squash: apply the PR's file versions onto the current base tree
	headTree := r.treeAtRef(headTip)
	newRoot := curBaseTree
	for _, fc := range prFiles {
		if fc.status == "deleted" {
			newRoot, _ = r.setInTree(newRoot, splitPath(fc.path), "")
		} else {
			blobOid := r.blobAtPath(headTree, fc.path)
			newRoot, _ = r.setInTree(newRoot, splitPath(fc.path), blobOid)
		}
	}
	ident := viewerIdentity(s.store.scen)
	msg := "Merge pull request #" + itoa(pr.number) + " from " + pr.head
	commit := buildCommit(newRoot, []string{baseTip}, msg, ident, ident, fixedTimestamp)
	mergeSha := r.objects.put("commit", commit)
	r.refs["refs/heads/"+pr.base] = mergeSha
	r.invalidatePackCache()
	pr.state = "closed"
	pr.merged = true
	pr.mergedAt = fixedISO
	pr.mergeSha = mergeSha
	return mergeSha, 0, msg
}

// prMergeable reports the GraphQL mergeable string for a PR.
func (s *server) prMergeable(r *Repo, pr *pull) string {
	if pr.state != "open" {
		return "UNKNOWN"
	}
	headTip := r.refs["refs/heads/"+pr.head]
	baseTip := r.refs["refs/heads/"+pr.base]
	if headTip == "" || baseTip == "" {
		return "UNKNOWN"
	}
	prFiles := r.pullFiles(pr)
	baseChanges := r.diffTrees(r.treeAtRef(pr.baseSha), r.treeAtRef(baseTip))
	changed := map[string]bool{}
	for _, fc := range baseChanges {
		changed[fc.path] = true
	}
	for _, fc := range prFiles {
		if changed[fc.path] {
			return "CONFLICTING"
		}
	}
	return "MERGEABLE"
}

func (s *server) restPullMerge(rq *request, r *Repo, n int) *response {
	pr := s.findPull(r, n)
	if pr == nil {
		return notFoundJSON("Not Found")
	}
	if rq.method != "PUT" {
		return notFoundJSON("")
	}
	mergeSha, status, msg := s.mergePull(r, pr)
	if status != 0 {
		return errJSON(status, msg)
	}
	var w jw
	w.raw(`{"merged":true,"sha":`)
	w.str(mergeSha)
	w.raw(`,"message":`)
	w.str(msg)
	w.raw(`}`)
	return jsonResp(200, w.b)
}

func (s *server) restPullCommits(rq *request, r *Repo, n int) *response {
	pr := s.findPull(r, n)
	if pr == nil {
		return notFoundJSON("Not Found")
	}
	headTip := r.refs["refs/heads/"+pr.head]
	baseTip := r.refs["refs/heads/"+pr.base]
	var w jw
	w.raw("[")
	first := true
	for _, coid := range r.commitList(headTip) {
		if coid == baseTip {
			break
		}
		o := r.objects[coid]
		if o == nil {
			continue
		}
		cv, ok := parseCommit(o.data)
		if !ok {
			continue
		}
		if !first {
			w.raw(",")
		}
		first = false
		w.raw(`{"sha":`)
		w.str(coid)
		w.raw(`,"commit":{"message":`)
		w.str(cv.message)
		w.raw(`}}`)
	}
	w.raw("]")
	return jsonResp(200, w.b)
}

// restDispatch routes /api/v3 requests. Caller holds the store lock.
func (s *server) restDispatch(rq *request) *response {
	segs := rq.segments
	if len(segs) == 2 {
		// /api/v3 or /api/v3/
		if rq.method == "GET" {
			return s.restRoot(rq)
		}
		return notFoundJSON("")
	}
	if len(segs) >= 3 && segs[2] == "meta" {
		if rq.method == "GET" {
			return s.restMeta(rq)
		}
		return notFoundJSON("")
	}
	if len(segs) >= 3 && segs[2] == "user" {
		if len(segs) == 3 {
			if rq.method == "GET" {
				return jsonResp(200, userJSON(s.store.scen))
			}
			return notFoundJSON("")
		}
		if len(segs) == 4 && segs[3] == "repos" {
			return s.restUserRepos(rq)
		}
		return notFoundJSON("")
	}
	if len(segs) >= 3 && segs[2] == "repos" {
		r, resp := s.requireRepo(segs, 3)
		if resp != nil {
			return resp
		}
		if len(segs) == 5 {
			return s.restRepo(rq, segs)
		}
		if len(segs) >= 6 {
			switch segs[5] {
			case "branches":
				if rq.method == "GET" && len(segs) == 6 {
					return s.restBranches(rq, r)
				}
			case "commits":
				if rq.method == "GET" && len(segs) == 6 {
					return s.restCommits(rq, r)
				}
			case "contents":
				return s.restContents(rq, r, segs)
			case "labels":
				if len(segs) == 6 {
					return s.restLabels(rq, r)
				}
			case "issues":
				if len(segs) == 6 {
					return s.restIssues(rq, r)
				}
				if len(segs) == 7 {
					if n, err := strconv.Atoi(segs[6]); err == nil {
						return s.restIssueOne(rq, r, n)
					}
					return notFoundJSON("")
				}
				if len(segs) == 8 && segs[7] == "comments" {
					if n, err := strconv.Atoi(segs[6]); err == nil {
						return s.restIssueComments(rq, r, n)
					}
					return notFoundJSON("")
				}
			case "pulls":
				if len(segs) == 6 {
					return s.restPulls(rq, r)
				}
				if len(segs) == 7 {
					if n, err := strconv.Atoi(segs[6]); err == nil {
						return s.restPullOne(rq, r, n)
					}
					return notFoundJSON("")
				}
				if len(segs) == 8 {
					if n, err := strconv.Atoi(segs[6]); err == nil {
						switch segs[7] {
						case "files":
							if rq.method == "GET" {
								return s.restPullFiles(rq, r, n)
							}
						case "merge":
							return s.restPullMerge(rq, r, n)
						case "commits":
							if rq.method == "GET" {
								return s.restPullCommits(rq, r, n)
							}
						}
					}
					return notFoundJSON("")
				}
			case "git":
				if len(segs) == 7 && segs[6] == "refs" {
					if rq.method == "POST" {
						return s.restCreateRef(rq, r)
					}
				}
			}
		}
		return notFoundJSON("")
	}
	return notFoundJSON("")
}
