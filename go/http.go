// Raw TCP HTTP/1.1 server: hand-rolled for speed and binary size.
// One goroutine per connection; keep-alive by default; responses are
// built fully in memory so Content-Length is always exact.
package main

import (
	"bufio"
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
	headers map[string]string // lower-cased keys
	body    []byte
	host    string
	// parsed helpers
	segments []string
}

func (rq *request) header(name string) string {
	return rq.headers[strings.ToLower(name)]
}

func (rq *request) bearer() string {
	h := rq.header("authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return h[len("Bearer "):]
	}
	return ""
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
	line, err := readLine(br)
	if err != nil {
		return nil, false, err
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, errBadRequest
	}
	rq := &request{method: parts[0], headers: map[string]string{}}
	rawURL := parts[1]
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		rq.rawPath = rawURL[:i]
		rq.query = rawURL[i+1:]
	} else {
		rq.rawPath = rawURL
	}
	rq.path = urlDecodePath(rq.rawPath)
	rq.segments = splitPath(rq.path)
	for {
		hl, err := readLine(br)
		if err != nil {
			return nil, false, err
		}
		if hl == "" {
			break
		}
		colon := strings.IndexByte(hl, ':')
		if colon < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(hl[:colon]))
		val := strings.TrimSpace(hl[colon+1:])
		if prev, ok := rq.headers[key]; ok {
			rq.headers[key] = prev + ", " + val
		} else {
			rq.headers[key] = val
		}
	}
	rq.host = rq.header("host")
	if strings.Contains(strings.ToLower(rq.header("expect")), "100-continue") {
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
	if strings.Contains(strings.ToLower(rq.header("connection")), "close") {
		keepAlive = false
	}
	if parts[2] == "HTTP/1.0" && !strings.Contains(strings.ToLower(rq.header("connection")), "keep-alive") {
		keepAlive = false
	}
	return rq, keepAlive, nil
}

func readBody(br *bufio.Reader, rq *request) ([]byte, error) {
	if te := strings.ToLower(rq.header("transfer-encoding")); strings.Contains(te, "chunked") {
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
	cl := rq.header("content-length")
	if cl == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(cl))
	if err != nil || n < 0 || n > 64*1024*1024 {
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
