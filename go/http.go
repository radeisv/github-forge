// Raw TCP HTTP/1.1 server: hand-rolled for speed and binary size.
// One goroutine per connection; keep-alive by default; responses are
// built fully in memory so Content-Length is always exact.
package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
)

type request struct {
	method  string
	path    string // decoded path, no query
	query   string
	rawPath string
	host    string
	// Headers, parsed once into fields. The old code kept a
	// map[string]string and lower-cased every name and value on
	// every lookup (authorization, host, expect, content-length,
	// transfer-encoding, connection, git-protocol), so one request
	// paid for a dozen small allocations before dispatch ran.
	auth           string // raw Authorization value
	contentLength  int    // -1 when absent
	chunked        bool
	expectContinue bool
	connClose      bool
	connKeepAlive  bool
	gitProtocol    string
	http10         bool
	body           []byte
	// parsed helpers
	segments []string
}

func (rq *request) bearer() string {
	h := rq.auth
	if strings.HasPrefix(h, "Bearer ") {
		return h[len("Bearer "):]
	}
	return ""
}

// foldEq reports whether b equals s ignoring ASCII case (no allocation).
func foldEq(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != s[i] {
			return false
		}
	}
	return true
}

// containsFold reports whether b contains sub ignoring ASCII case.
func containsFold(b []byte, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(b); i++ {
		if foldEq(b[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

// parseUint parses a decimal Content-Length value; false on any
// non-digit (the old code's Atoi failed the same way).
func parseUint(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 64*1024*1024 {
			return 0, false
		}
	}
	return n, true
}

// trimCRLF strips one trailing \n and one trailing \r from a line.
func trimCRLF(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return b
}

// readLineSlice reads one LF-terminated line without allocating: the
// slice aliases the bufio buffer and is valid until the next read, so
// callers copy what they keep. A line longer than the buffer falls
// back to an accumulating read.
func readLineSlice(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err == nil {
		return line, nil
	}
	if err == io.EOF {
		if len(line) == 0 {
			return nil, io.EOF
		}
		return line, nil
	}
	if err != bufio.ErrBufferFull {
		return nil, err
	}
	out := append([]byte(nil), line...)
	for err == bufio.ErrBufferFull {
		line, err = br.ReadSlice('\n')
		out = append(out, line...)
	}
	if err == io.EOF {
		return out, nil
	}
	return out, err
}

// response is a fully built HTTP response.
type response struct {
	status      int
	contentType string
	body        []byte
	extra       [][2]string // extra headers, in order
	noBody      bool        // 204-style: no body at all
}

var reqCounter uint64

func nextReqID() string {
	n := atomic.AddUint64(&reqCounter, 1)
	return "req-" + itoa(int(n))
}

type server struct {
	store *Store
	ln    net.Listener
}

func (s *server) serveConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 65536)
	for {
		rq, keepAlive, err := readRequestConn(conn, br)
		if err != nil {
			return
		}
		resp := s.dispatch(rq)
		if !writeResponse(conn, resp) {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// readRequestConn reads one request, answering 100-continue inline when the
// client asks for it. keepAlive reports whether to serve another request.
func readRequestConn(conn net.Conn, br *bufio.Reader) (*request, bool, error) {
	line, err := readLineSlice(br)
	if err != nil {
		return nil, false, err
	}
	line = trimCRLF(line)
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 < 0 {
		return nil, false, errBadRequest
	}
	sp2 := bytes.IndexByte(line[sp1+1:], ' ')
	if sp2 < 0 {
		return nil, false, errBadRequest
	}
	sp2 += sp1 + 1
	rq := &request{method: string(line[:sp1]), contentLength: -1}
	rawURL := line[sp1+1 : sp2]
	rq.http10 = string(line[sp2+1:]) == "HTTP/1.0"
	if i := bytes.IndexByte(rawURL, '?'); i >= 0 {
		rq.rawPath = string(rawURL[:i])
		rq.query = string(rawURL[i+1:])
	} else {
		rq.rawPath = string(rawURL)
	}
	rq.path = urlDecodePath(rq.rawPath)
	rq.segments = splitPath(rq.path)
	for {
		hl, err := readLineSlice(br)
		if err != nil {
			return nil, false, err
		}
		hl = trimCRLF(hl)
		if len(hl) == 0 {
			break
		}
		colon := bytes.IndexByte(hl, ':')
		if colon < 0 {
			continue
		}
		key := bytes.TrimSpace(hl[:colon])
		val := bytes.TrimSpace(hl[colon+1:])
		switch {
		case foldEq(key, "authorization"):
			rq.auth = string(val)
		case foldEq(key, "host"):
			rq.host = string(val)
		case foldEq(key, "content-length"):
			n, ok := parseUint(val)
			if !ok {
				return nil, false, errBadRequest
			}
			rq.contentLength = n
		case foldEq(key, "transfer-encoding"):
			if containsFold(val, "chunked") {
				rq.chunked = true
			}
		case foldEq(key, "expect"):
			if containsFold(val, "100-continue") {
				rq.expectContinue = true
			}
		case foldEq(key, "connection"):
			if containsFold(val, "close") {
				rq.connClose = true
			}
			if containsFold(val, "keep-alive") {
				rq.connKeepAlive = true
			}
		case foldEq(key, "git-protocol"):
			rq.gitProtocol = string(val)
		}
	}
	if rq.expectContinue {
		conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}
	// body
	body, err := readBody(br, rq)
	if err != nil {
		return nil, false, err
	}
	rq.body = body
	// keep-alive: HTTP/1.1 default on unless Connection: close; HTTP/1.0
	// default off unless Connection: keep-alive.
	keepAlive := true
	if rq.connClose {
		keepAlive = false
	}
	if rq.http10 && !rq.connKeepAlive {
		keepAlive = false
	}
	return rq, keepAlive, nil
}

func readBody(br *bufio.Reader, rq *request) ([]byte, error) {
	if rq.chunked {
		var out []byte
		for {
			line, err := readLine(br)
			if err != nil {
				return nil, err
			}
			// chunk size may carry extensions after ';'
			if i := strings.IndexByte(line, ';'); i >= 0 {
				line = line[:i]
			}
			n, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
			if err != nil {
				return nil, errBadRequest
			}
			if n == 0 {
				// trailing headers until blank line
				for {
					tl, err := readLine(br)
					if err != nil {
						return nil, err
					}
					if tl == "" {
						break
					}
				}
				return out, nil
			}
			if n < 0 || n > 64*1024*1024 {
				return nil, errBadRequest
			}
			chunk := make([]byte, n)
			if _, err := readFull(br, chunk); err != nil {
				return nil, err
			}
			out = append(out, chunk...)
			// trailing CRLF
			if _, err := readLine(br); err != nil {
				return nil, err
			}
		}
	}
	if rq.contentLength < 0 {
		return nil, nil
	}
	n := rq.contentLength
	if n > 64*1024*1024 {
		return nil, errBadRequest
	}
	if n == 0 {
		return nil, nil
	}
	body := make([]byte, n)
	if _, err := readFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
}

func readFull(br *bufio.Reader, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := br.Read(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// readLine reads one CRLF- or LF-terminated line (without the terminator).
func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, nil
}

var errBadRequest = &httpError{msg: "bad request"}

type httpError struct{ msg string }

func (e *httpError) Error() string { return e.msg }

// writeResponse writes the full response in one Write call when possible.
func writeResponse(conn net.Conn, resp *response) bool {
	var buf []byte
	buf = append(buf, []byte("HTTP/1.1 ")...)
	buf = append(buf, []byte(itoa(resp.status))...)
	buf = append(buf, ' ')
	buf = append(buf, []byte(statusText(resp.status))...)
	buf = append(buf, []byte("\r\n")...)
	if resp.contentType != "" {
		buf = append(buf, []byte("Content-Type: "+resp.contentType+"\r\n")...)
	}
	buf = append(buf, []byte("X-GitHub-Request-Id: "+nextReqID()+"\r\n")...)
	for _, h := range resp.extra {
		buf = append(buf, []byte(h[0]+": "+h[1]+"\r\n")...)
	}
	if resp.noBody {
		buf = append(buf, []byte("Content-Length: 0\r\n\r\n")...)
	} else {
		buf = append(buf, []byte("Content-Length: "+itoa(len(resp.body))+"\r\n\r\n")...)
		buf = append(buf, resp.body...)
	}
	_, err := conn.Write(buf)
	return err == nil
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	}
	return "Status"
}

// urlDecodePath percent-decodes a path, leaving '+' literal.
func urlDecodePath(p string) string {
	var out []byte
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '%' && i+2 < len(p) {
			hi, ok1 := hexVal(p[i+1])
			lo, ok2 := hexVal(p[i+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				i += 2
				continue
			}
		}
		out = append(out, c)
	}
	return string(out)
}

// queryParam returns the first value of key in the query string.
func (rq *request) queryParam(key string) string {
	q := rq.query
	for len(q) > 0 {
		var pair string
		if i := strings.IndexByte(q, '&'); i >= 0 {
			pair, q = q[:i], q[i+1:]
		} else {
			pair, q = q, ""
		}
		if i := strings.IndexByte(pair, '='); i >= 0 {
			if pair[:i] == key {
				return urlDecodeParam(pair[i+1:])
			}
		} else if pair == key {
			return ""
		}
	}
	return ""
}

func urlDecodeParam(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '+' {
			out = append(out, ' ')
			continue
		}
		if c == '%' && i+2 < len(s) {
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				i += 2
				continue
			}
		}
		out = append(out, c)
	}
	return string(out)
}

// ---- response helpers ----

const jsonCT = "application/json; charset=utf-8"

func jsonResp(status int, body []byte, extra ...[2]string) *response {
	return &response{status: status, contentType: jsonCT, body: body, extra: extra}
}

func errJSON(status int, msg string) *response {
	var w jw
	w.raw(`{"message":`)
	w.str(msg)
	w.raw(`}`)
	return jsonResp(status, w.b)
}

func notFoundJSON(msg string) *response {
	if msg == "" {
		msg = "Not Found"
	}
	return errJSON(404, msg)
}
