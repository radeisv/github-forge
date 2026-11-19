// Git object model: SHA-1 ids, loose-object encoding, tree/commit/tag
// build and parse. Byte layouts match the kit's git-client.ts exactly.
package main

import (
	"bytes"
	"crypto/sha1"
	"sort"
)

const zeroOID = "0000000000000000000000000000000000000000"

const identity = "Conformance <conformance@make.fun>"

// fixedTimestamp is the Unix time every server-generated commit/tag carries.
// No wall clock ever enters a stored object, so runs are deterministic.
const fixedTimestamp = 1700000000

// obj is one git object.
type obj struct {
	typ  string // "commit" | "tree" | "blob" | "tag"
	data []byte
}

func hashObject(typ string, data []byte) string {
	// Header "type len\0" built on the stack: the old code allocated
	// three throwaway []byte slices per hash, and a push hashes every
	// object it stores.
	var hdr [48]byte
	n := copy(hdr[:], typ)
	hdr[n] = ' '
	n++
	n += copy(hdr[n:], itoa(len(data)))
	hdr[n] = 0
	n++
	h := sha1.New()
	h.Write(hdr[:n])
	h.Write(data)
	sum := h.Sum(nil)
	return hexEncode(sum)
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xF])
	}
	return string(out)
}

func hexDecode(s string) ([]byte, bool) {
	if len(s)%2 != 0 {
		return nil, false
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, ok1 := hexVal(s[2*i])
		lo, ok2 := hexVal(s[2*i+1])
		if !ok1 || !ok2 {
			return nil, false
		}
		out[i] = hi<<4 | lo
	}
	return out, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ---- trees ----

type treeEntry struct {
	mode string // "100644" or "40000"
	name string
	oid  string
}

func compareTreeNames(a, b treeEntry) int {
	an, bn := a.name, b.name
	if a.mode == "40000" {
		an += "/"
	}
	if b.mode == "40000" {
		bn += "/"
	}
	return bytes.Compare([]byte(an), []byte(bn))
}

func buildTree(entries []treeEntry) []byte {
	sorted := append([]treeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf []byte
	for _, e := range sorted {
		raw, ok := hexDecode(e.oid)
		if !ok {
			continue
		}
		buf = append(buf, []byte(e.mode+" "+e.name)...)
		buf = append(buf, 0)
		buf = append(buf, raw...)
	}
	return buf
}

func parseTree(data []byte) ([]treeEntry, bool) {
	var out []treeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, false
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, false
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, false
		}
		mode := string(data[off:sp])
		name := string(data[sp+1 : nul])
		oid := hexEncode(data[nul+1 : nul+21])
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
		off = nul + 21
	}
	return out, true
}

// ---- commits ----

type commitView struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

func parseCommit(data []byte) (commitView, bool) {
	var cv commitView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
		cv.message = string(data[blank+2:])
	}
	for _, line := range bytes.Split(data[:headerEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		var key, val string
		if sp < 0 {
			key, val = string(line), ""
		} else {
			key, val = string(line[:sp]), string(line[sp+1:])
		}
		switch key {
		case "tree":
			cv.tree = val
		case "parent":
			cv.parents = append(cv.parents, val)
		case "author":
			cv.author = val
		case "committer":
			cv.committer = val
		}
	}
	if !isOID(cv.tree) {
		return cv, false
	}
	for _, p := range cv.parents {
		if !isOID(p) {
			return cv, false
		}
	}
	return cv, true
}

// buildCommit renders a commit object. message is used verbatim.
func buildCommit(tree string, parents []string, message, author, committer string, timestamp int) []byte {
	var buf []byte
	buf = append(buf, []byte("tree "+tree+"\n")...)
	for _, p := range parents {
		buf = append(buf, []byte("parent "+p+"\n")...)
	}
	who := author
	if who == "" {
		who = identity
	}
	whoC := committer
	if whoC == "" {
		whoC = identity
	}
	buf = append(buf, []byte("author "+who+" "+itoa(timestamp)+" +0000\n")...)
	buf = append(buf, []byte("committer "+whoC+" "+itoa(timestamp)+" +0000\n")...)
	buf = append(buf, '\n')
	buf = append(buf, []byte(message)...)
	return buf
}

// ---- tags ----

type tagView struct {
	object  string
	typ     string
	tag     string
	tagger  string
	message string
}

func parseTag(data []byte) (tagView, bool) {
	var tv tagView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
		tv.message = string(data[blank+2:])
	}
	for _, line := range bytes.Split(data[:headerEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			return tv, false
		}
		key, val := string(line[:sp]), string(line[sp+1:])
		switch key {
		case "object":
			tv.object = val
		case "type":
			tv.typ = val
		case "tag":
			tv.tag = val
		case "tagger":
			tv.tagger = val
		}
	}
	if !isOID(tv.object) {
		return tv, false
	}
	switch tv.typ {
	case "commit", "tree", "blob", "tag":
	default:
		return tv, false
	}
	return tv, true
}

// ---- object store helpers (per repo) ----

// objStore is a repo's objects: oid -> object.
type objStore map[string]*obj

func (s objStore) put(typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := s[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		s[oid] = &obj{typ: typ, data: cp}
	}
	return oid
}

func (s objStore) get(oid string) *obj {
	return s[oid]
}

// ancestors returns true when anc is reachable from tip following parents
// (including tip == anc). Tags are peeled first by the caller.
func (s objStore) isAncestor(anc, tip string) bool {
	if anc == tip {
		return true
	}
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o := s[cur]
		if o == nil || o.typ != "commit" {
			continue
		}
		cv, ok := parseCommit(o.data)
		if !ok {
			continue
		}
		stack = append(stack, cv.parents...)
	}
	return false
}

// peel follows tag objects to their target and reports the target oid and
// its type. Non-tag oids return themselves.
func (s objStore) peel(oid string) (string, string) {
	for i := 0; i < 16; i++ {
		o := s[oid]
		if o == nil || o.typ != "tag" {
			return oid, typeOf(s, oid)
		}
		tv, ok := parseTag(o.data)
		if !ok {
			return oid, "tag"
		}
		oid = tv.object
	}
	return oid, typeOf(s, oid)
}

func typeOf(s objStore, oid string) string {
	if o := s[oid]; o != nil {
		return o.typ
	}
	return ""
}

// reachable returns every object oid reachable from the given commit/tag
// oids (commits, their trees recursively, and blobs), in traversal order
// (deduplicated). parents are followed unless stopAt is non-nil and the
// commit is in stopAt (the boundary itself is not traversed further, but
// the boundary commit IS included when includeBoundary is true).
func (s objStore) reachable(roots []string, stopAt map[string]bool, includeBoundary bool) []string {
	var out []string
	seen := map[string]bool{}
	var walkCommit func(oid string)
	var walkTree func(oid string)
	walkTree = func(oid string) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		o := s[oid]
		if o == nil || o.typ != "tree" {
			return
		}
		out = append(out, oid)
		entries, ok := parseTree(o.data)
		if !ok {
			return
		}
		for _, e := range entries {
			child := s[e.oid]
			if child == nil {
				continue
			}
			if child.typ == "tree" {
				walkTree(e.oid)
			} else if !seen[e.oid] {
				seen[e.oid] = true
				out = append(out, e.oid)
			}
		}
	}
	walkCommit = func(oid string) {
		// peel tags first
		peeled, _ := s.peel(oid)
		if peeled != oid {
			if !seen[oid] {
				seen[oid] = true
				out = append(out, oid)
			}
			oid = peeled
		}
		if seen[oid] {
			return
		}
		o := s[oid]
		if o == nil || o.typ != "commit" {
			return
		}
		if stopAt != nil && stopAt[oid] && !includeBoundary {
			return
		}
		seen[oid] = true
		out = append(out, oid)
		cv, ok := parseCommit(o.data)
		if !ok {
			return
		}
		walkTree(cv.tree)
		if stopAt != nil && stopAt[oid] {
			return
		}
		for _, p := range cv.parents {
			walkCommit(p)
		}
	}
	for _, r := range roots {
		walkCommit(r)
	}
	return out
}
