// In-memory store: repositories, git objects, refs, issues, pull requests,
// labels and comments. One store serves REST, GraphQL, MCP and git alike.
package main

import (
	"encoding/base64"
	"strings"
	"sync"
)

// fixedISO is the timestamp every created/updated/merged field carries.
// No wall clock ever enters a stored object, so runs are deterministic.
const fixedISO = "2025-01-01T00:00:00Z"

func viewerIdentity(s *scenario) string {
	return s.viewer.name + " <" + s.viewer.email + ">"
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func repoGID(id int) string    { return b64("Repository:" + itoa(id)) }
func issueGID(repoID, n int) string {
	return b64("Issue:" + itoa(repoID) + ":" + itoa(n))
}
func pullGID(repoID, n int) string { return b64("PullRequest:" + itoa(repoID) + ":" + itoa(n)) }
func commentGID(repoID, issueN, cID int) string {
	return b64("IssueComment:" + itoa(repoID) + ":" + itoa(issueN) + ":" + itoa(cID))
}
func userGID(id int) string { return b64("User:" + itoa(id)) }

// decodeGID decodes a global id into kind + parts.
func decodeGID(gid string) (string, []string, bool) {
	raw, err := base64.StdEncoding.DecodeString(gid)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}

// ---------------------------------------------------------------- entities

type label struct {
	id    int
	name  string
	color string
}

type comment struct {
	id   int
	gid  string
	body string
	seq  int
}

type issue struct {
	repo     *Repo
	number   int
	id       int
	gid      string
	title    string
	body     string
	state    string // "open" | "closed"
	labels   []string
	comments []*comment
	seq      int
	cached   []byte // REST JSON, invalidated on mutation
}

type pull struct {
	repo     *Repo
	number   int
	id       int
	gid      string
	title    string
	body     string
	head     string
	base     string
	baseSha  string // base tip when the PR was created
	state    string // "open" | "closed"
	merged   bool
	mergedAt string
	mergeSha string
	seq      int
}

type fileChange struct {
	path      string
	status    string // "added" | "modified" | "deleted"
	additions int
	deletions int
}

type Repo struct {
	id            int
	owner         string
	name          string
	description   string
	private       bool
	defaultBranch string
	refs          map[string]string // full refname -> tip oid
	objects       objStore
	labels        map[string]*label
	labelOrder    []string
	issues        []*issue
	pulls         []*pull
	nextIssue     int
	nextPull      int
	nextComment   int
	packCache     map[string][]byte
	cacheMu       sync.Mutex // guards packCache; store RLock is held by callers
	cachedRepo    []byte
}

// packCached returns the cached pack bytes for key, if present.
func (r *Repo) packCached(key string) ([]byte, bool) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	b, ok := r.packCache[key]
	return b, ok
}

func (r *Repo) packStore(key string, body []byte) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	r.packCache[key] = body
}

type Store struct {
	mu            sync.RWMutex
	repos         map[string]*Repo // owner + "/" + name -> repo
	repoList      []*Repo          // creation order
	nextRepoID    int
	nextIssueID   int
	nextPullID    int
	nextCommentID int
	nextLabelID   int
	token         string
	scen          *scenario
}

func newStore(token string, scen *scenario) *Store {
	return &Store{
		repos:         map[string]*Repo{},
		nextRepoID:    10000,
		nextIssueID:   1,
		nextPullID:    1,
		nextCommentID: 1,
		nextLabelID:   1,
		token:         token,
		scen:          scen,
	}
}

// lookup returns the repo for owner/name, or nil. Caller must hold at least
// a read lock. A repo under any owner other than the token's viewer is
// invisible (the suite expects 404, never 403, for foreign owners).
func (s *Store) lookup(owner, name string) *Repo {
	if owner != s.scen.viewer.login {
		return nil
	}
	return s.repos[owner+"/"+name]
}

// createRepo makes a repo owned by the viewer. Returns nil when a repo of
// that name already exists.
func (s *Store) createRepo(name, description string, private bool) *Repo {
	key := s.scen.viewer.login + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil
	}
	r := &Repo{
		id:            s.nextRepoID,
		owner:         s.scen.viewer.login,
		name:          name,
		description:   description,
		private:       private,
		defaultBranch: "main",
		refs:          map[string]string{},
		objects:       objStore{},
		labels:        map[string]*label{},
		packCache:     map[string][]byte{},
		nextIssue:     1,
		nextPull:      1,
		nextComment:   1,
	}
	s.nextRepoID++
	s.repos[key] = r
	s.repoList = append(s.repoList, r)
	return r
}

func (s *Store) deleteRepo(r *Repo) {
	delete(s.repos, r.owner+"/"+r.name)
	for i, x := range s.repoList {
		if x == r {
			s.repoList = append(s.repoList[:i], s.repoList[i+1:]...)
			break
		}
	}
}

// ---------------------------------------------------------------- tree surgery

// treeEntries returns the entries of the tree oid, or nil for an empty tree.
func (r *Repo) treeEntries(oid string) []treeEntry {
	if oid == "" {
		return nil
	}
	o := r.objects[oid]
	if o == nil || o.typ != "tree" {
		return nil
	}
	entries, ok := parseTree(o.data)
	if !ok {
		return nil
	}
	return entries
}

// setInTree returns the oid of the tree that results from setting (or, when
// blobOid is "", deleting) the file at segs inside treeOid. The second
// result reports whether the resulting tree is empty.
func (r *Repo) setInTree(treeOid string, segs []string, blobOid string) (string, bool) {
	entries := r.treeEntries(treeOid)
	if len(segs) == 1 {
		name := segs[0]
		out := entries[:0]
		found := false
		for _, e := range entries {
			if e.name == name {
				found = true
				if blobOid != "" {
					out = append(out, treeEntry{mode: "100644", name: name, oid: blobOid})
				}
				continue
			}
			out = append(out, e)
		}
		if !found && blobOid != "" {
			out = append(out, treeEntry{mode: "100644", name: name, oid: blobOid})
		}
		if len(out) == 0 {
			return "", true
		}
		return r.objects.put("tree", buildTree(out)), false
	}
	// descend into a subdirectory
	name := segs[0]
	var subOid string
	var rest []treeEntry
	for _, e := range entries {
		if e.name == name && e.mode == "40000" {
			subOid = e.oid
		} else {
			rest = append(rest, e)
		}
	}
	newSub, empty := r.setInTree(subOid, segs[1:], blobOid)
	if !empty {
		rest = append(rest, treeEntry{mode: "40000", name: name, oid: newSub})
	}
	if len(rest) == 0 {
		return "", true
	}
	return r.objects.put("tree", buildTree(rest)), false
}

// blobAtPath returns the blob oid at path inside the tree, or "".
func (r *Repo) blobAtPath(treeOid, path string) string {
	segs := splitPath(path)
	cur := treeOid
	for i, seg := range segs {
		entries := r.treeEntries(cur)
		var found *treeEntry
		for j := range entries {
			if entries[j].name == seg {
				found = &entries[j]
				break
			}
		}
		if found == nil {
			return ""
		}
		if i == len(segs)-1 {
			if found.mode == "100644" {
				return found.oid
			}
			return ""
		}
		if found.mode != "40000" {
			return ""
		}
		cur = found.oid
	}
	return ""
}

// treeAtRef resolves a branch name, full refname or commit oid to a root
// tree oid ("" when the ref is unborn or unknown).
func (r *Repo) treeAtRef(ref string) string {
	commitOid := r.commitAtRef(ref)
	if commitOid == "" {
		return ""
	}
	o := r.objects[commitOid]
	if o == nil || o.typ != "commit" {
		return ""
	}
	cv, ok := parseCommit(o.data)
	if !ok {
		return ""
	}
	return cv.tree
}

// commitAtRef resolves a branch name, full refname or commit oid to a
// commit oid, or "".
func (r *Repo) commitAtRef(ref string) string {
	if ref == "" {
		ref = r.defaultBranch
	}
	if isOID(ref) {
		if o := r.objects[ref]; o != nil && o.typ == "commit" {
			return ref
		}
		return ""
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	return r.refs[ref]
}

// listDir returns the entries of the directory at dirPath ("" = root).
func (r *Repo) listDir(treeOid, dirPath string) []treeEntry {
	if dirPath == "" || dirPath == "/" {
		return r.treeEntries(treeOid)
	}
	segs := splitPath(dirPath)
	cur := treeOid
	for _, seg := range segs {
		entries := r.treeEntries(cur)
		var next string
		for _, e := range entries {
			if e.name == seg && e.mode == "40000" {
				next = e.oid
				break
			}
		}
		if next == "" {
			return nil
		}
		cur = next
	}
	return r.treeEntries(cur)
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// commitList walks from tip following parents (newest first).
func (r *Repo) commitList(tip string) []string {
	var out []string
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		o := r.objects[cur]
		if o == nil || o.typ != "commit" {
			continue
		}
		out = append(out, cur)
		cv, ok := parseCommit(o.data)
		if !ok {
			continue
		}
		// parents in reverse so the first parent is visited first
		for i := len(cv.parents) - 1; i >= 0; i-- {
			stack = append(stack, cv.parents[i])
		}
	}
	return out
}

// diffTrees compares two root trees and returns the changed files.
func (r *Repo) diffTrees(oldRoot, newRoot string) []fileChange {
	var out []fileChange
	r.diffTreeInto(oldRoot, newRoot, "", &out)
	return out
}

func (r *Repo) diffTreeInto(oldOid, newOid, prefix string, out *[]fileChange) {
	oldEntries := r.treeEntries(oldOid)
	newEntries := r.treeEntries(newOid)
	oldByName := map[string]treeEntry{}
	for _, e := range oldEntries {
		oldByName[e.name] = e
	}
	newByName := map[string]treeEntry{}
	for _, e := range newEntries {
		newByName[e.name] = e
	}
	for _, ne := range newEntries {
		path := prefix + ne.name
		oe, ok := oldByName[ne.name]
		if !ok {
			if ne.mode == "40000" {
				r.diffTreeInto("", ne.oid, path+"/", out)
			} else {
				*out = append(*out, fileChange{path: path, status: "added", additions: countLines(r.blobBytes(ne.oid))})
			}
			continue
		}
		if oe.oid == ne.oid && oe.mode == ne.mode {
			continue
		}
		if ne.mode == "40000" && oe.mode == "40000" {
			r.diffTreeInto(oe.oid, ne.oid, path+"/", out)
		} else if ne.mode == "40000" || oe.mode == "40000" {
			// type change: treat as delete + add
			if oe.mode == "40000" {
				r.diffTreeInto(oe.oid, "", path+"/", out)
			} else {
				*out = append(*out, fileChange{path: path, status: "deleted", deletions: countLines(r.blobBytes(oe.oid))})
			}
			if ne.mode == "40000" {
				r.diffTreeInto("", ne.oid, path+"/", out)
			} else {
				*out = append(*out, fileChange{path: path, status: "added", additions: countLines(r.blobBytes(ne.oid))})
			}
		} else {
			*out = append(*out, fileChange{path: path, status: "modified", additions: countLines(r.blobBytes(ne.oid)), deletions: countLines(r.blobBytes(oe.oid))})
		}
	}
	for _, oe := range oldEntries {
		if _, ok := newByName[oe.name]; !ok {
			path := prefix + oe.name
			if oe.mode == "40000" {
				r.diffTreeInto(oe.oid, "", path+"/", out)
			} else {
				*out = append(*out, fileChange{path: path, status: "deleted", deletions: countLines(r.blobBytes(oe.oid))})
			}
		}
	}
}

func (r *Repo) blobBytes(oid string) []byte {
	if o := r.objects[oid]; o != nil && o.typ == "blob" {
		return o.data
	}
	return nil
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// invalidatePackCache clears the repo's cached packs after any mutation.
func (r *Repo) invalidatePackCache() {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	r.packCache = map[string][]byte{}
}

// ---------------------------------------------------------------- REST JSON

// cachedUserJSON is the viewer user object, constant for the process
// (one store, one scenario). Built once, reused by every renderer.
var cachedUserJSON []byte

func userJSON(s *scenario) []byte {
	if cachedUserJSON != nil {
		return cachedUserJSON
	}
	var w jw
	w.raw(`{"login":`)
	w.str(s.viewer.login)
	w.raw(`,"id":`)
	w.int(s.viewer.id)
	w.raw(`,"node_id":`)
	w.str(userGID(s.viewer.id))
	w.raw(`,"type":"User","site_admin":false}`)
	cachedUserJSON = w.b
	return w.b
}

// repoJSON renders the repository object (GitHub REST shape, minimum
// assertions plus the usual fields).
func repoJSON(s *scenario, r *Repo) []byte {
	if r.cachedRepo != nil {
		return r.cachedRepo
	}
	var w jw
	w.raw(`{"id":`)
	w.int(r.id)
	w.raw(`,"node_id":`)
	w.str(repoGID(r.id))
	w.raw(`,"name":`)
	w.str(r.name)
	w.raw(`,"full_name":`)
	w.str(r.owner + "/" + r.name)
	w.raw(`,"owner":`)
	w.bytes(userJSON(s))
	w.raw(`,"private":`)
	w.bool(r.private)
	w.raw(`,"description":`)
	if r.description == "" {
		w.null()
	} else {
		w.str(r.description)
	}
	w.raw(`,"default_branch":`)
	w.str(r.defaultBranch)
	w.raw(`,"created_at":`)
	w.str(fixedISO)
	w.raw(`,"updated_at":`)
	w.str(fixedISO)
	w.raw(`,"pushed_at":`)
	w.str(fixedISO)
	w.raw(`,"html_url":`)
	w.str("https://example.invalid/" + r.owner + "/" + r.name)
	w.raw(`,"clone_url":`)
	w.str("https://example.invalid/" + r.owner + "/" + r.name + ".git")
	w.raw(`,"homepage":null,"language":null,"forks_count":0,"stargazers_count":0,"watchers_count":0,"open_issues_count":`)
	w.int(len(r.issues))
	w.raw(`,"has_issues":true,"has_projects":true,"has_wiki":true,"archived":false,"disabled":false,"visibility":`)
	if r.private {
		w.str("private")
	} else {
		w.str("public")
	}
	w.raw(`,"license":null}`)
	r.cachedRepo = w.b
	return w.b
}

func labelJSON(l *label, repoID int) []byte {
	var w jw
	w.raw(`{"id":`)
	w.int(l.id)
	w.raw(`,"node_id":`)
	w.str(b64("Label:" + itoa(repoID) + ":" + itoa(l.id)))
	w.raw(`,"name":`)
	w.str(l.name)
	w.raw(`,"color":`)
	w.str(l.color)
	w.raw(`,"description":"","default":false}`)
	return w.b
}

// issueJSON renders one issue; the result is cached on the issue.
func issueJSON(s *scenario, r *Repo, is *issue) []byte {
	if is.cached != nil {
		return is.cached
	}
	var w jw
	w.raw(`{"id":`)
	w.int(is.id)
	w.raw(`,"node_id":`)
	w.str(is.gid)
	w.raw(`,"number":`)
	w.int(is.number)
	w.raw(`,"title":`)
	w.str(is.title)
	w.raw(`,"body":`)
	if is.body == "" {
		w.null()
	} else {
		w.str(is.body)
	}
	w.raw(`,"state":`)
	w.str(is.state)
	w.raw(`,"labels":[`)
	for i, name := range is.labels {
		if i > 0 {
			w.raw(",")
		}
		if l, ok := r.labels[name]; ok {
			w.bytes(labelJSON(l, r.id))
		} else {
			w.raw(`{"name":`)
			w.str(name)
			w.raw(`,"color":"ededed"}`)
		}
	}
	w.raw(`],"user":`)
	w.bytes(userJSON(s))
	w.raw(`,"assignees":[],"created_at":`)
	w.str(fixedISO)
	w.raw(`,"updated_at":`)
	w.str(fixedISO)
	w.raw(`,"closed_at":`)
	if is.state == "closed" {
		w.str(fixedISO)
	} else {
		w.null()
	}
	w.raw(`,"comments":`)
	w.int(len(is.comments))
	w.raw(`,"html_url":`)
	w.str("https://example.invalid/" + r.owner + "/" + r.name + "/issues/" + itoa(is.number))
	w.raw(`}`)
	is.cached = w.b
	return w.b
}

func commentJSON(s *scenario, r *Repo, is *issue, c *comment) []byte {
	var w jw
	w.raw(`{"id":`)
	w.int(c.id)
	w.raw(`,"node_id":`)
	w.str(c.gid)
	w.raw(`,"body":`)
	w.str(c.body)
	w.raw(`,"user":`)
	w.bytes(userJSON(s))
	w.raw(`,"created_at":`)
	w.str(fixedISO)
	w.raw(`,"updated_at":`)
	w.str(fixedISO)
	w.raw(`,"html_url":`)
	w.str("https://example.invalid/" + r.owner + "/" + r.name + "/issues/" + itoa(is.number) + "#issuecomment-" + itoa(c.id))
	w.raw(`}`)
	return w.b
}

func pullJSON(s *scenario, r *Repo, pr *pull) []byte {
	headSha := r.refs["refs/heads/"+pr.head]
	baseSha := r.refs["refs/heads/"+pr.base]
	var w jw
	w.raw(`{"id":`)
	w.int(pr.id)
	w.raw(`,"node_id":`)
	w.str(pr.gid)
	w.raw(`,"number":`)
	w.int(pr.number)
	w.raw(`,"title":`)
	w.str(pr.title)
	w.raw(`,"body":`)
	if pr.body == "" {
		w.null()
	} else {
		w.str(pr.body)
	}
	w.raw(`,"state":`)
	w.str(pr.state)
	w.raw(`,"merged":`)
	w.bool(pr.merged)
	w.raw(`,"merged_at":`)
	if pr.merged {
		w.str(pr.mergedAt)
	} else {
		w.null()
	}
	w.raw(`,"merge_commit_sha":`)
	if pr.mergeSha == "" {
		w.null()
	} else {
		w.str(pr.mergeSha)
	}
	w.raw(`,"mergeable":true,"draft":false,"head":{"ref":`)
	w.str(pr.head)
	w.raw(`,"sha":`)
	w.str(headSha)
	w.raw(`,"label":`)
	w.str(r.owner + ":" + pr.head)
	w.raw(`,"user":`)
	w.bytes(userJSON(s))
	w.raw(`},"base":{"ref":`)
	w.str(pr.base)
	w.raw(`,"sha":`)
	w.str(baseSha)
	w.raw(`,"label":`)
	w.str(r.owner + ":" + pr.base)
	w.raw(`,"user":`)
	w.bytes(userJSON(s))
	w.raw(`},"user":`)
	w.bytes(userJSON(s))
	w.raw(`,"created_at":`)
	w.str(fixedISO)
	w.raw(`,"updated_at":`)
	w.str(fixedISO)
	w.raw(`,"closed_at":`)
	if pr.state == "closed" {
		w.str(fixedISO)
	} else {
		w.null()
	}
	w.raw(`,"labels":[],"html_url":`)
	w.str("https://example.invalid/" + r.owner + "/" + r.name + "/pull/" + itoa(pr.number))
	w.raw(`}`)
	return w.b
}
