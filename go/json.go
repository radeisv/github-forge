// Minimal JSON: a small parser for request bodies and a writer for
// responses. Hand-rolled (no encoding/json) for binary size and speed;
// the hot read path never parses JSON at all (cached bytes).
package main

import "strconv"

// jval is a parsed JSON value: nil, bool, float64, string, []jval, map[string]jval.
type jval = interface{}

type jparser struct {
	b []byte
	i int
}

func parseJSON(b []byte) (jval, bool) {
	p := &jparser{b: b}
	p.ws()
	v, ok := p.value()
	if !ok {
		return nil, false
	}
	p.ws()
	return v, p.i == len(p.b)
}

func (p *jparser) ws() {
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.i++
		} else {
			break
		}
	}
}

func (p *jparser) value() (jval, bool) {
	if p.i >= len(p.b) {
		return nil, false
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, ok := p.str()
		return s, ok
	case c == 't':
		return p.lit("true", true)
	case c == 'f':
		return p.lit("false", false)
	case c == 'n':
		return p.lit("null", nil)
	default:
		return p.number()
	}
}

func (p *jparser) lit(word string, v jval) (jval, bool) {
	if len(p.b)-p.i < len(word) || string(p.b[p.i:p.i+len(word)]) != word {
		return nil, false
	}
	p.i += len(word)
	return v, true
}

func (p *jparser) number() (jval, bool) {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	for p.i < len(p.b) {
		c := p.b[p.i]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.i++
		} else {
			break
		}
	}
	if p.i == start {
		return nil, false
	}
	f, err := strconv.ParseFloat(string(p.b[start:p.i]), 64)
	if err != nil {
		return nil, false
	}
	return f, true
}

func (p *jparser) str() (string, bool) {
	if p.i >= len(p.b) || p.b[p.i] != '"' {
		return "", false
	}
	p.i++
	var out []byte
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			p.i++
			return string(out), true
		}
		if c == '\\' {
			p.i++
			if p.i >= len(p.b) {
				return "", false
			}
			e := p.b[p.i]
			p.i++
			switch e {
			case '"':
				out = append(out, '"')
			case '\\':
				out = append(out, '\\')
			case '/':
				out = append(out, '/')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				if p.i+4 > len(p.b) {
					return "", false
				}
				n, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 32)
				if err != nil {
					return "", false
				}
				p.i += 4
				r := rune(n)
				if r >= 0xD800 && r <= 0xDBFF && p.i+6 <= len(p.b) && p.b[p.i] == '\\' && p.b[p.i+1] == 'u' {
					n2, err2 := strconv.ParseUint(string(p.b[p.i+2:p.i+6]), 16, 32)
					if err2 == nil {
						r2 := rune(n2)
						if r2 >= 0xDC00 && r2 <= 0xDFFF {
							r = 0x10000 + ((r - 0xD800) << 10) + (r2 - 0xDC00)
							p.i += 6
						}
					}
				}
				out = appendRune(out, r)
			default:
				return "", false
			}
			continue
		}
		out = append(out, c)
		p.i++
	}
	return "", false
}

func appendRune(b []byte, r rune) []byte {
	if r < 0x80 {
		return append(b, byte(r))
	}
	if r < 0x800 {
		return append(b, 0xC0|byte(r>>6), 0x80|byte(r&0x3F))
	}
	if r < 0x10000 {
		return append(b, 0xE0|byte(r>>12), 0x80|byte((r>>6)&0x3F), 0x80|byte(r&0x3F))
	}
	return append(b, 0xF0|byte(r>>18), 0x80|byte((r>>12)&0x3F), 0x80|byte((r>>6)&0x3F), 0x80|byte(r&0x3F))
}

func (p *jparser) object() (jval, bool) {
	p.i++ // {
	m := map[string]jval{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return m, true
	}
	for {
		p.ws()
		k, ok := p.str()
		if !ok {
			return nil, false
		}
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, false
		}
		p.i++
		p.ws()
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		m[k] = v
		p.ws()
		if p.i >= len(p.b) {
			return nil, false
		}
		if p.b[p.i] == '}' {
			p.i++
			return m, true
		}
		if p.b[p.i] != ',' {
			return nil, false
		}
		p.i++
	}
}

func (p *jparser) array() (jval, bool) {
	p.i++ // [
	a := []jval{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return a, true
	}
	for {
		p.ws()
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		a = append(a, v)
		p.ws()
		if p.i >= len(p.b) {
			return nil, false
		}
		if p.b[p.i] == ']' {
			p.i++
			return a, true
		}
		if p.b[p.i] != ',' {
			return nil, false
		}
		p.i++
	}
}

// ---- typed accessors ----

func jobj(v jval) (map[string]jval, bool) {
	m, ok := v.(map[string]jval)
	return m, ok
}

func jstr(v jval) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func jarr(v jval) ([]jval, bool) {
	a, ok := v.([]jval)
	return a, ok
}

func jnum(v jval) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func jbool(v jval) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// objStr returns the string field or "".
func objStr(m map[string]jval, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// objStrOK returns the string field and whether it was present and a string.
func objStrOK(m map[string]jval, key string) (string, bool) {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

// objInt returns the integer value of a numeric field (0 if absent/not numeric).
func objInt(m map[string]jval, key string) int {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return 0
}

// objBool returns the boolean field (false if absent/not boolean).
func objBool(m map[string]jval, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// ---- writer ----

// jw builds JSON text.
type jw struct{ b []byte }

func (w *jw) raw(s string)     { w.b = append(w.b, s...) }
func (w *jw) bytes(b []byte)   { w.b = append(w.b, b...) }
func (w *jw) str(s string)     { writeJSONString(&w.b, s) }
func (w *jw) int(n int)        { w.b = strconv.AppendInt(w.b, int64(n), 10) }
func (w *jw) bool(v bool) {
	if v {
		w.b = append(w.b, "true"...)
	} else {
		w.b = append(w.b, "false"...)
	}
}
func (w *jw) null() { w.b = append(w.b, "null"...) }

func writeJSONString(b *[]byte, s string) {
	*b = append(*b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			*b = append(*b, '\\', '"')
		case '\\':
			*b = append(*b, '\\', '\\')
		case '\n':
			*b = append(*b, '\\', 'n')
		case '\r':
			*b = append(*b, '\\', 'r')
		case '\t':
			*b = append(*b, '\\', 't')
		case '\b':
			*b = append(*b, '\\', 'b')
		case '\f':
			*b = append(*b, '\\', 'f')
		default:
			if c < 0x20 {
				*b = append(*b, '\\', 'u', '0', '0', hexDigit(c>>4), hexDigit(c&0xF))
			} else {
				*b = append(*b, c)
			}
		}
	}
	*b = append(*b, '"')
}

func hexDigit(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + n - 10
}

// jsonEscape returns s as a JSON string literal (with quotes).
func jsonEscape(s string) string {
	var b []byte
	writeJSONString(&b, s)
	return string(b)
}
