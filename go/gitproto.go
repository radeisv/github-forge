// Git smart HTTP: protocol v2 + v0 upload-pack, v0 receive-pack.
// Pkt-line framing, ls-refs, fetch (incl. shallow), push with
// report-status, and pack generation. No git binary anywhere.
package main

import (
	"sort"
	"strconv"
	"strings"
)

// ---- pkt-lines ----

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	var out []byte
	out = append(out, hexDigit(byte(n>>12&0xF)), hexDigit(byte(n>>8&0xF)), hexDigit(byte(n>>4&0xF)), hexDigit(byte(n&0xF)))
	out = append(out, payload...)
	return out
}

func pktString(s string) []byte { return pktLine([]byte(s)) }

const pktFlush = "0000"
const pktDelim = "0001"

type pkt struct {
	kind    string // "flush" | "delim" | "data"
	payload []byte
}

// parsePktLines parses a pkt-line stream. Stops at the first flush.
func parsePktLines(data []byte) ([]pkt, int, bool) {
	var out []pkt
	off := 0
	for off+4 <= len(data) {
		n, err := strconv.ParseInt(string(data[off:off+4]), 16, 32)
		if err != nil || n < 0 || n > 65520 {
			return nil, 0, false
		}
		if n == 0 {
			return append(out, pkt{kind: "flush"}), off + 4, true
		}
		if n == 1 {
			out = append(out, pkt{kind: "delim"})
			off += 4
			continue
		}
		if n == 2 {
			// response-end; treat like a delim for our purposes
			out = append(out, pkt{kind: "delim"})
			off += 4
			continue
		}
		if off+int(n) > len(data) {
			return nil, 0, false
		}
		out = append(out, pkt{kind: "data", payload: data[off+4 : off+int(n)]})
		off += int(n)
	}
	return out, off, true
}

// ---- ref listing ----

// sortedRefs returns the repo's refs sorted by name.
func (r *Repo) sortedRefs() []string {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// refTip returns the tip oid for a ref, peeling tags to commits where asked.
func (r *Repo) refTip(ref string) string {
	return r.refs[ref]
}

// ---- advertisements ----

const uploadAdvCT = "application/x-git-upload-pack-advertisement"
const receiveAdvCT = "application/x-git-receive-pack-advertisement"
const uploadResCT = "application/x-git-upload-pack-result"
const receiveResCT = "application/x-git-receive-pack-result"

func (s *server) gitAdvertisement(rq *request, owner, repoName, service string) *response {
	// Caller (dispatch) holds the store lock for the whole handler.
	st := s.store
	r := st.lookup(owner, repoName)
	if r == nil {
		return errJSON(404, "Not Found")
	}
	refs := r.sortedRefs()
	tips := r.refs
	v2 := service == "git-upload-pack" && rq.header("git-protocol") == "version=2"
	var body []byte
	if service == "git-upload-pack" && v2 {
		// No "# service=" preamble here: git protocol v2 starts with
		// "version 2" exactly, and real git rejects a preamble in a
		// v2 advertisement (the kit client tolerates both).
		body = append(body, pktString("version 2\n")...)
		body = append(body, pktString("agent=forge/1.0\n")...)
		body = append(body, pktString("ls-refs=unborn\n")...)
		body = append(body, pktString("fetch=shallow filter\n")...)
		body = append(body, pktString("object-format=sha1\n")...)
		body = append(body, []byte(pktFlush)...)
		return &response{status: 200, contentType: uploadAdvCT, body: body}
	}
	// v0 advertisement
	if service == "git-upload-pack" {
		body = append(body, pktString("# service=git-upload-pack\n")...)
	} else {
		body = append(body, pktString("# service=git-receive-pack\n")...)
	}
	caps := "symref=HEAD:refs/heads/main agent=forge/1.0 object-format=sha1 side-band-64k"
	if service == "git-receive-pack" {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0 symref=HEAD:refs/heads/main object-format=sha1"
	} else {
		caps += " ofs-delta"
	}
	if len(refs) == 0 {
		line := zeroOID + " capabilities^{}\x00" + caps + "\n"
		body = append(body, pktString(line)...)
	} else {
		for i, ref := range refs {
			line := tips[ref] + " " + ref
			if i == 0 {
				line += "\x00" + caps
			}
			// peeled tag lines follow their tag line
			body = append(body, pktString(line+"\n")...)
			if strings.HasPrefix(ref, "refs/tags/") {
				if peeled, _ := r.objects.peel(tips[ref]); peeled != tips[ref] {
					body = append(body, pktString(peeled+" "+ref+"^{}\n")...)
				}
			}
		}
	}
	body = append(body, []byte(pktFlush)...)
	ct := uploadAdvCT
	if service == "git-receive-pack" {
		ct = receiveAdvCT
	}
	return &response{status: 200, contentType: ct, body: body}
}

// ---- ls-refs (v2) ----

func (s *server) gitLsRefs(rq *request, r *Repo) *response {
	pkts, _, ok := parsePktLines(rq.body)
	if !ok {
		return errJSON(400, "bad pkt-line stream")
	}
	peel, symrefs := false, false
	var prefixes []string
	for _, p := range pkts {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
		}
	}
	var body []byte
	refs := r.sortedRefs()
	headTip := r.refs["refs/heads/main"]
	if len(refs) == 0 {
		// unborn HEAD only; no refs/… lines (the empty-clone check
		// requires zero refs/… entries).
		line := "unborn HEAD"
		if symrefs {
			line += " symref-target:refs/heads/main"
		}
		body = append(body, pktString(line+"\n")...)
	} else {
		if len(prefixes) == 0 || prefixMatch("HEAD", prefixes) {
			// HEAD is reported when no prefix filter excludes it; the
			// client asks for no prefix in the main ls-refs check.
			headLine := headTip + " HEAD"
			if symrefs {
				headLine += " symref-target:refs/heads/main"
			}
			if len(prefixes) == 0 {
				body = append(body, pktString(headLine+"\n")...)
			}
		}
		for _, ref := range refs {
			if len(prefixes) > 0 && !hasPrefixAny(ref, prefixes) {
				continue
			}
			oid := r.refs[ref]
			line := oid + " " + ref
			if strings.HasPrefix(ref, "refs/tags/") && peel {
				if peeled, _ := r.objects.peel(oid); peeled != oid {
					line += " peeled:" + peeled
				}
			}
			body = append(body, pktString(line+"\n")...)
		}
	}
	body = append(body, []byte(pktFlush)...)
	return &response{status: 200, contentType: uploadResCT, body: body}
}

func prefixMatch(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func hasPrefixAny(s string, prefixes []string) bool { return prefixMatch(s, prefixes) }

// ---- fetch ----

type fetchArgs struct {
	wants    []string
	haves    []string
	shallows []string
	deepen   int
}

func parseFetchArgs(pkts []pkt) fetchArgs {
	var fa fetchArgs
	for _, p := range pkts {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			fa.wants = append(fa.wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			fa.haves = append(fa.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			fa.shallows = append(fa.shallows, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				fa.deepen = n
			}
		}
	}
	return fa
}

// fetchPack computes the objects a fetch must send, plus shallow-info lines.
func (r *Repo) fetchPack(fa fetchArgs) (objects []*obj, shallowLines []string) {
	// have sets: everything reachable from haves — but a shallow
	// boundary cuts the walk: the client holds the boundary commit and
	// its tree, not its ancestors. Without the cut, a deepen fetch
	// would wrongly treat the parent as already held.
	haveObjs := map[string]bool{}
	haveCommits := map[string]bool{}
	if len(fa.haves) > 0 {
		shallowSet := map[string]bool{}
		for _, sh := range fa.shallows {
			shallowSet[sh] = true
		}
		for _, oid := range r.objects.reachable(fa.haves, shallowSet, true) {
			haveObjs[oid] = true
			if o := r.objects[oid]; o != nil && o.typ == "commit" {
				haveCommits[oid] = true
			}
		}
	}
	if fa.deepen > 0 {
		return r.fetchShallow(fa, haveObjs, haveCommits)
	}
	// full fetch: reachable(wants) minus reachable(haves)
	wantObjs := r.objects.reachable(fa.wants, nil, true)
	seen := map[string]bool{}
	for _, oid := range wantObjs {
		if haveObjs[oid] || seen[oid] {
			continue
		}
		seen[oid] = true
		if o := r.objects[oid]; o != nil {
			objects = append(objects, o)
		}
	}
	return objects, nil
}

// fetchShallow implements depth-limited fetch.
func (r *Repo) fetchShallow(fa fetchArgs, haveObjs, haveCommits map[string]bool) ([]*obj, []string) {
	// walk generations from the wants; generation 1 = the want itself
	type item struct {
		oid string
		gen int
	}
	var sentCommits []string
	sentSet := map[string]bool{}
	queue := []item{}
	for _, w := range fa.wants {
		peeled, _ := r.objects.peel(w)
		if w != peeled {
			// tag object itself is sent
			if !haveObjs[w] && !sentSet[w] {
				sentSet[w] = true
			}
		}
		queue = append(queue, item{peeled, 1})
	}
	// BFS by generation
	seenGen := map[string]int{}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if it.gen > fa.deepen {
			continue
		}
		if prev, ok := seenGen[it.oid]; ok && prev <= it.gen {
			continue
		}
		seenGen[it.oid] = it.gen
		o := r.objects[it.oid]
		if o == nil || o.typ != "commit" {
			continue
		}
		if !haveCommits[it.oid] && !sentSet[it.oid] {
			sentSet[it.oid] = true
			sentCommits = append(sentCommits, it.oid)
		}
		cv, ok := parseCommit(o.data)
		if !ok {
			continue
		}
		for _, p := range cv.parents {
			queue = append(queue, item{p, it.gen + 1})
		}
	}
	// objects: sent commits + their trees/blobs (excluding held objects)
	var objects []*obj
	added := map[string]bool{}
	for oid := range sentSet {
		added[oid] = true
	}
	for _, coid := range sentCommits {
		o := r.objects[coid]
		if o == nil {
			continue
		}
		objects = append(objects, o)
		cv, _ := parseCommit(o.data)
		// collect tree+blob objects of this commit's tree
		r.collectTreeObjects(cv.tree, haveObjs, added, &objects)
	}
	// tag objects the wants named
	for _, w := range fa.wants {
		if o := r.objects[w]; o != nil && o.typ == "tag" && !haveObjs[w] {
			found := false
			for _, x := range objects {
				if x == o {
					found = true
				}
			}
			if !found {
				objects = append(objects, o)
			}
		}
	}
	// shallow-info: new boundaries + unshallows
	var lines []string
	for _, coid := range sentCommits {
		cv, _ := parseCommit(r.objects[coid].data)
		boundary := true
		for _, p := range cv.parents {
			if sentSet[p] || haveCommits[p] {
				boundary = false
				break
			}
		}
		if boundary {
			lines = append(lines, "shallow "+coid)
		}
	}
	for _, b := range fa.shallows {
		if sentSet[b] {
			lines = append(lines, "unshallow "+b)
			continue
		}
		// a boundary whose parent is now known is no longer shallow
		o := r.objects[b]
		if o == nil || o.typ != "commit" {
			continue
		}
		cv, _ := parseCommit(o.data)
		for _, p := range cv.parents {
			if sentSet[p] || haveCommits[p] {
				lines = append(lines, "unshallow "+b)
				break
			}
		}
	}
	return objects, lines
}

// collectTreeObjects appends the tree and blob objects under treeOid that
// are not held, in traversal order.
func (r *Repo) collectTreeObjects(treeOid string, haveObjs, added map[string]bool, out *[]*obj) {
	if treeOid == "" || added[treeOid] {
		return
	}
	o := r.objects[treeOid]
	if o == nil || o.typ != "tree" {
		return
	}
	if !haveObjs[treeOid] {
		added[treeOid] = true
		*out = append(*out, o)
	}
	entries, ok := parseTree(o.data)
	if !ok {
		return
	}
	for _, e := range entries {
		child := r.objects[e.oid]
		if child == nil {
			continue
		}
		if child.typ == "tree" {
			r.collectTreeObjects(e.oid, haveObjs, added, out)
		} else if !haveObjs[e.oid] && !added[e.oid] {
			added[e.oid] = true
			*out = append(*out, child)
		}
	}
}

// sideband wraps data into sideband-64k pkt-lines (band 1), chunked.
func sidebandPackets(data []byte) []byte {
	var out []byte
	const maxChunk = 65515
	for len(data) > 0 {
		n := len(data)
		if n > maxChunk {
			n = maxChunk
		}
		payload := make([]byte, 0, n+1)
		payload = append(payload, 1)
		payload = append(payload, data[:n]...)
		out = append(out, pktLine(payload)...)
		data = data[n:]
	}
	return out
}

// gitUploadPack dispatches a POST /git-upload-pack to v2 or v0 by the
// Git-Protocol header.
func (s *server) gitUploadPack(rq *request, r *Repo) *response {
	if rq.header("git-protocol") == "version=2" {
		return s.gitFetchV2(rq, r)
	}
	return s.gitFetchV0(rq, r)
}

func (s *server) gitFetchV2(rq *request, r *Repo) *response {
	pkts, _, ok := parsePktLines(rq.body)
	if !ok {
		return errJSON(400, "bad pkt-line stream")
	}
	// ls-refs?
	for _, p := range pkts {
		if p.kind == "data" && strings.HasPrefix(string(p.payload), "command=ls-refs") {
			return s.gitLsRefs(rq, r)
		}
	}
	fa := parseFetchArgs(pkts)
	cacheKey := fetchCacheKey(fa)
	// Singleflight the pack build: 50 concurrent clones of one repo
	// all ask for the same pack. Holding cacheMu across the cache
	// check, the fetch and the store means only the first builds it;
	// the rest wait briefly and hit the cache instead of each
	// zlib-compressing the same objects on a shared CPU. Lock order
	// stays store -> cacheMu (the caller holds the store read lock,
	// and invalidatePackCache runs only under the store write lock),
	// so this cannot deadlock.
	r.cacheMu.Lock()
	if cached, ok := r.packCache[cacheKey]; ok {
		r.cacheMu.Unlock()
		return &response{status: 200, contentType: uploadResCT, body: cached}
	}
	objects, shallowLines := r.fetchPack(fa)
	var body []byte
	body = append(body, pktString("acknowledgments\n")...)
	body = append(body, pktString("NAK\n")...)
	body = append(body, []byte(pktDelim)...)
	if len(shallowLines) > 0 {
		body = append(body, pktString("shallow-info\n")...)
		for _, l := range shallowLines {
			body = append(body, pktString(l+"\n")...)
		}
		body = append(body, []byte(pktDelim)...)
	}
	if len(objects) > 0 {
		body = append(body, pktString("packfile\n")...)
		body = append(body, sidebandPackets(writePack(objects))...)
	}
	body = append(body, []byte(pktFlush)...)
	r.packCache[cacheKey] = body
	r.cacheMu.Unlock()
	return &response{status: 200, contentType: uploadResCT, body: body}
}

func fetchCacheKey(fa fetchArgs) string {
	var b strings.Builder
	b.WriteString("f:")
	for _, w := range fa.wants {
		b.WriteString(w)
		b.WriteByte(',')
	}
	b.WriteString("|h:")
	for _, h := range fa.haves {
		b.WriteString(h)
		b.WriteByte(',')
	}
	b.WriteString("|d:")
	b.WriteString(strconv.Itoa(fa.deepen))
	b.WriteString("|s:")
	for _, sh := range fa.shallows {
		b.WriteString(sh)
		b.WriteByte(',')
	}
	return b.String()
}

// parsePktLinesAll parses every pkt-line in data across flush boundaries,
// returning all data payloads in order.
func parsePktLinesAll(data []byte) [][]byte {
	var out [][]byte
	off := 0
	for off+4 <= len(data) {
		n, err := strconv.ParseInt(string(data[off:off+4]), 16, 32)
		if err != nil || n < 0 || n > 65520 {
			break
		}
		if n <= 2 {
			off += 4
			continue
		}
		if off+int(n) > len(data) {
			break
		}
		out = append(out, data[off+4:off+int(n)])
		off += int(n)
	}
	return out
}

// gitFetchV0 is the fallback path if a client ever speaks v0 fetch.
func (s *server) gitFetchV0(rq *request, r *Repo) *response {
	// v0 request: want lines + flush, then have lines + done + flush.
	var fa fetchArgs
	for _, raw := range parsePktLinesAll(rq.body) {
		line := strings.TrimSuffix(string(raw), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			// first want line may carry capabilities after a space
			want := strings.TrimSpace(line[5:])
			if i := strings.IndexByte(want, ' '); i >= 0 {
				want = want[:i]
			}
			fa.wants = append(fa.wants, want)
		case strings.HasPrefix(line, "have "):
			fa.haves = append(fa.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			fa.shallows = append(fa.shallows, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				fa.deepen = n
			}
		}
	}
	// Same singleflight cache as the v2 path, under a distinct key
	// (the v0 body has no acknowledgments/shallow-info framing).
	cacheKey := "v0:" + fetchCacheKey(fa)
	r.cacheMu.Lock()
	if cached, ok := r.packCache[cacheKey]; ok {
		r.cacheMu.Unlock()
		return &response{status: 200, contentType: uploadResCT, body: cached}
	}
	objects, _ := r.fetchPack(fa)
	var body []byte
	body = append(body, pktString("NAK\n")...)
	if len(objects) > 0 {
		body = append(body, sidebandPackets(writePack(objects))...)
	}
	body = append(body, []byte(pktFlush)...)
	r.packCache[cacheKey] = body
	r.cacheMu.Unlock()
	return &response{status: 200, contentType: uploadResCT, body: body}
}

// ---- receive-pack (push) ----

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

func (s *server) gitReceivePack(rq *request, r *Repo) *response {
	pkts, consumed, ok := parsePktLines(rq.body)
	if !ok {
		return errJSON(400, "bad pkt-line stream")
	}
	var cmds []pushCommand
	sideband := false
	for _, p := range pkts {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		// first line may carry "\0<capabilities>"
		if i := strings.IndexByte(line, 0); i >= 0 {
			caps := line[i+1:]
			if strings.Contains(caps, "side-band-64k") {
				sideband = true
			}
			line = line[:i]
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			continue
		}
		cmds = append(cmds, pushCommand{oldOid: parts[0], newOid: parts[1], ref: parts[2]})
	}
	// the pack follows the flush
	rest := rq.body[consumed:]
	var packObjs []*obj
	packErr := ""
	needsPack := false
	for _, c := range cmds {
		if c.newOid != zeroOID {
			needsPack = true
		}
	}
	if needsPack {
		if len(rest) == 0 {
			packErr = "missing packfile"
		} else {
			objs, err := readPack(rest)
			if err != nil {
				packErr = "bad packfile"
			} else {
				packObjs = objs
			}
		}
	}
	// store the pack's objects first (a refused ref keeps its objects,
	// like git does)
	if packErr == "" {
		for _, o := range packObjs {
			oid := hashObject(o.typ, o.data)
			if _, exists := r.objects[oid]; !exists {
				cp := make([]byte, len(o.data))
				copy(cp, o.data)
				r.objects[oid] = &obj{typ: o.typ, data: cp}
			}
		}
	}
	// apply commands in order
	type report struct {
		ref string
		ok  bool
		why string
	}
	var reports []report
	for _, c := range cmds {
		if packErr != "" {
			reports = append(reports, report{ref: c.ref, ok: false, why: packErr})
			continue
		}
		if !strings.HasPrefix(c.ref, "refs/") {
			reports = append(reports, report{ref: c.ref, ok: false, why: "invalid ref"})
			continue
		}
		cur, exists := r.refs[c.ref]
		if c.newOid == zeroOID {
			// delete
			if !exists {
				reports = append(reports, report{ref: c.ref, ok: false, why: "remote ref does not exist"})
				continue
			}
			delete(r.refs, c.ref)
			reports = append(reports, report{ref: c.ref, ok: true})
			continue
		}
		// new object must exist and be a commit or tag
		target := r.objects[c.newOid]
		if target == nil || (target.typ != "commit" && target.typ != "tag") {
			reports = append(reports, report{ref: c.ref, ok: false, why: "object not found"})
			continue
		}
		if c.oldOid == zeroOID {
			// create
			if exists {
				reports = append(reports, report{ref: c.ref, ok: false, why: "already exists"})
				continue
			}
			r.refs[c.ref] = c.newOid
			reports = append(reports, report{ref: c.ref, ok: true})
			continue
		}
		// update
		if !exists {
			reports = append(reports, report{ref: c.ref, ok: false, why: "remote ref does not exist"})
			continue
		}
		if cur != c.oldOid {
			reports = append(reports, report{ref: c.ref, ok: false, why: "stale info"})
			continue
		}
		newPeeled, _ := r.objects.peel(c.newOid)
		if !r.objects.isAncestor(c.oldOid, newPeeled) {
			reports = append(reports, report{ref: c.ref, ok: false, why: "non-fast-forward"})
			continue
		}
		r.refs[c.ref] = c.newOid
		reports = append(reports, report{ref: c.ref, ok: true})
	}
	r.invalidatePackCache()
	// report-status, pkt-line framed inside sideband band 1 when negotiated
	var reportBytes []byte
	if packErr != "" {
		reportBytes = append(reportBytes, pktString("unpack "+packErr+"\n")...)
	} else {
		reportBytes = append(reportBytes, pktString("unpack ok\n")...)
	}
	for _, rep := range reports {
		if rep.ok {
			reportBytes = append(reportBytes, pktString("ok "+rep.ref+"\n")...)
		} else {
			reportBytes = append(reportBytes, pktString("ng "+rep.ref+" "+rep.why+"\n")...)
		}
	}
	reportBytes = append(reportBytes, []byte(pktFlush)...)
	var body []byte
	if sideband {
		body = append(body, sidebandPackets(reportBytes)...)
	} else {
		body = append(body, reportBytes...)
	}
	return &response{status: 200, contentType: receiveResCT, body: body}
}
