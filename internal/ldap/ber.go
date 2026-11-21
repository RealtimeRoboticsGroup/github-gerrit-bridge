// Package ldap is the little of LDAPv3 (RFC 4511) the bridge needs: an
// anonymous subtree search over TLS for one object class, and a Directory
// of GitHub accounts read from it (design §7.1). Its BER is only what
// those messages use, and every length is bounded.
package ldap

import (
	"errors"
	"fmt"
)

// BER tags used here (RFC 4511 §4, X.690).
const (
	tagBoolean     = 0x01
	tagInteger     = 0x02
	tagOctetString = 0x04
	tagEnumerated  = 0x0a
	tagSequence    = 0x30
	tagSet         = 0x31

	tagBindRequest     = 0x60 // [APPLICATION 0], constructed
	tagBindResponse    = 0x61 // [APPLICATION 1]
	tagSimpleAuth      = 0x80 // [0], primitive: simple authentication
	tagSearchRequest   = 0x63 // [APPLICATION 3], constructed
	tagSearchEntry     = 0x64 // [APPLICATION 4]
	tagSearchDone      = 0x65 // [APPLICATION 5]
	tagUnbindRequest   = 0x42 // [APPLICATION 2], primitive
	tagSearchReference = 0x73 // [APPLICATION 19]
	tagExtendedResp    = 0x78 // [APPLICATION 24]

	tagFilterAnd      = 0xa0 // [0], constructed
	tagFilterEquality = 0xa3 // [3], constructed
	tagControls       = 0xa0 // [0] in an LDAPMessage
)

// maxMessage bounds one LDAPMessage: an entry here is a few short
// attributes.
const maxMessage = 1 << 20

// element is one BER TLV: its tag, and its contents.
type element struct {
	tag  byte
	body []byte
}

// encode is e's BER (definite lengths, as LDAP requires).
func encode(tag byte, body []byte) []byte {
	out := []byte{tag}
	switch n := len(body); {
	case n < 0x80:
		out = append(out, byte(n))
	case n <= 0xff:
		out = append(out, 0x81, byte(n))
	case n <= 0xffff:
		out = append(out, 0x82, byte(n>>8), byte(n))
	default:
		out = append(out, 0x83, byte(n>>16), byte(n>>8), byte(n))
	}
	return append(out, body...)
}

func encodeString(tag byte, s string) []byte { return encode(tag, []byte(s)) }

// encodeInt is a non-negative INTEGER (or ENUMERATED) with tag.
func encodeInt(tag byte, v int64) []byte {
	if v < 0 {
		panic("ldap: negative integer")
	}
	var b []byte
	for {
		b = append([]byte{byte(v)}, b...)
		v >>= 8
		if v == 0 {
			break
		}
	}
	if b[0]&0x80 != 0 {
		b = append([]byte{0}, b...) // stays positive
	}
	return encode(tag, b)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// errTruncated is input that ends inside an element.
var errTruncated = errors.New("ldap: truncated message")

// header reads a TLV header from b: the tag, the contents' length, and
// how long the header is. It refuses indefinite and over-long lengths,
// and multi-byte tags, none of which LDAP's messages use.
func header(b []byte) (tag byte, n, hlen int, err error) {
	if len(b) < 2 {
		return 0, 0, 0, errTruncated
	}
	tag = b[0]
	if tag&0x1f == 0x1f {
		return 0, 0, 0, fmt.Errorf("ldap: multi-byte tag 0x%x", tag)
	}
	l := int(b[1])
	if l < 0x80 {
		return tag, l, 2, nil
	}
	k := l & 0x7f
	if k == 0 || k > 4 { // four bytes, as some servers write even short lengths
		return 0, 0, 0, fmt.Errorf("ldap: unsupported length form 0x%x", l)
	}
	if len(b) < 2+k {
		return 0, 0, 0, errTruncated
	}
	n = 0
	for _, c := range b[2 : 2+k] {
		n = n<<8 | int(c)
	}
	if n > maxMessage {
		return 0, 0, 0, fmt.Errorf("ldap: element of %d bytes is too long", n)
	}
	return tag, n, 2 + k, nil
}

// next splits the first element off b.
func next(b []byte) (element, []byte, error) {
	tag, n, hlen, err := header(b)
	if err != nil {
		return element{}, nil, err
	}
	if len(b) < hlen+n {
		return element{}, nil, errTruncated
	}
	return element{tag, b[hlen : hlen+n]}, b[hlen+n:], nil
}

// children splits a constructed element's contents into its elements.
func children(body []byte) ([]element, error) {
	var out []element
	for len(body) > 0 {
		e, rest, err := next(body)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		body = rest
	}
	return out, nil
}

// intValue is a non-negative INTEGER or ENUMERATED element's value.
func intValue(e element) (int64, error) {
	if len(e.body) == 0 || len(e.body) > 8 || e.body[0]&0x80 != 0 {
		return 0, fmt.Errorf("ldap: bad integer % x", e.body)
	}
	var v int64
	for _, c := range e.body {
		v = v<<8 | int64(c)
	}
	return v, nil
}
