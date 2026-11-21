package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// Server is an LDAP server reached over TLS (ldaps://), whose certificate
// must chain to RootCAs (the system's if nil). Verification is never
// turned off.
type Server struct {
	// Addr is host:port; ServerName the name its certificate must have.
	Addr, ServerName string
	RootCAs          *x509.CertPool
	// Timeout bounds a whole search, connection included (30s if 0).
	Timeout time.Duration
}

// ParseServer reads an ldaps:// URL (no path, query or credentials:
// binds are anonymous) and, if caFile is set, the PEM certificates to
// trust for it instead of the system's.
func ParseServer(raw, caFile string) (*Server, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("ldap url %q: %w", raw, err)
	}
	if u.Scheme != "ldaps" {
		return nil, fmt.Errorf("ldap url %q: must be ldaps:// (the directory says who may act as whom)", raw)
	}
	if u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("ldap url %q: only ldaps://host[:port]", raw)
	}
	port := u.Port()
	if port == "" {
		port = "636"
	}
	s := &Server{Addr: net.JoinHostPort(u.Hostname(), port), ServerName: u.Hostname()}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("ldap ca_file: %w", err)
		}
		s.RootCAs = x509.NewCertPool()
		if !s.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ldap ca_file %s: no PEM certificates in it", caFile)
		}
	}
	return s, nil
}

// Entry is a search result: its DN, and its attributes' values by
// attribute name (lower-cased: LDAP's names are case-insensitive).
type Entry struct {
	DN    string
	Attrs map[string][]string
}

// ResultError is a search's result other than success.
type ResultError struct {
	Code    int64
	Message string
}

func (e *ResultError) Error() string {
	msg := fmt.Sprintf("ldap: result code %d", e.Code)
	if e.Code == 4 {
		msg += " (size limit exceeded: the server returns no more entries anonymously)"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// maxEntries bounds one search.
const maxEntries = 100000

// Search runs an anonymous subtree search under base for entries whose
// objectClass is class, returning attrs of each. Any result but success,
// a referral, or anything it does not understand is an error: the caller
// must not take a partial answer for the whole directory.
func (s *Server) Search(ctx context.Context, base, class string, attrs []string) ([]Entry, error) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := tls.Dialer{Config: &tls.Config{ServerName: s.ServerName, RootCAs: s.RootCAs, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return nil, fmt.Errorf("ldap: connecting to %s: %w", s.Addr, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	// An anonymous bind first: LDAPv3 allows searching without one, but
	// not every server does.
	if _, err := conn.Write(bindRequest(1)); err != nil {
		return nil, fmt.Errorf("ldap: binding: %w", err)
	}
	if err := readBind(conn); err != nil {
		return nil, err
	}
	if _, err := conn.Write(searchRequest(2, base, class, attrs)); err != nil {
		return nil, fmt.Errorf("ldap: sending the search: %w", err)
	}
	var out []Entry
	for {
		msg, err := readMessage(conn)
		if err != nil {
			return nil, err
		}
		id, op, err := splitMessage(msg)
		if err != nil {
			return nil, err
		}
		if op.tag == tagExtendedResp && id == 0 {
			return nil, errors.New("ldap: the server is closing the connection (notice of disconnection)")
		}
		if id != 2 {
			return nil, fmt.Errorf("ldap: a reply to message %d, not the search", id)
		}
		switch op.tag {
		case tagSearchEntry:
			e, err := parseEntry(op.body)
			if err != nil {
				return nil, err
			}
			if out = append(out, e); len(out) > maxEntries {
				return nil, fmt.Errorf("ldap: more than %d entries", maxEntries)
			}
		case tagSearchReference:
			return nil, errors.New("ldap: the server refers to another server (a referral), which is not followed")
		case tagSearchDone:
			code, msg, err := parseResult(op.body)
			if err != nil {
				return nil, err
			}
			if code != 0 {
				return nil, &ResultError{Code: code, Message: msg}
			}
			conn.Write(encode(tagSequence, concat(encodeInt(tagInteger, 3), []byte{tagUnbindRequest, 0})))
			return out, nil
		default:
			return nil, fmt.Errorf("ldap: unexpected reply 0x%x", op.tag)
		}
	}
}

// bindRequest is LDAPMessage id: an anonymous simple bind (RFC 4511
// §4.2: version 3, no name, an empty simple password).
func bindRequest(id int64) []byte {
	return encode(tagSequence, concat(encodeInt(tagInteger, id),
		encode(tagBindRequest, concat(encodeInt(tagInteger, 3), encodeString(tagOctetString, ""), encode(tagSimpleAuth, nil)))))
}

// readBind reads the bind's answer, which must be success.
func readBind(r io.Reader) error {
	msg, err := readMessage(r)
	if err != nil {
		return err
	}
	id, op, err := splitMessage(msg)
	if err != nil {
		return err
	}
	if id != 1 || op.tag != tagBindResponse {
		return fmt.Errorf("ldap: unexpected answer to the bind (message %d, 0x%x)", id, op.tag)
	}
	code, msgText, err := parseResult(op.body)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ResultError{Code: code, Message: "anonymous bind refused: " + msgText}
	}
	return nil
}

// searchRequest is LDAPMessage id: a SearchRequest (RFC 4511 §4.5.1) for
// a subtree search of base, never dereferencing aliases, without size or
// time limits of its own (the server's apply), for objectClass=class.
func searchRequest(id int64, base, class string, attrs []string) []byte {
	var sel []byte
	for _, a := range attrs {
		sel = append(sel, encodeString(tagOctetString, a)...)
	}
	req := encode(tagSearchRequest, concat(
		encodeString(tagOctetString, base),
		encodeInt(tagEnumerated, 2), // wholeSubtree
		encodeInt(tagEnumerated, 0), // neverDerefAliases
		encodeInt(tagInteger, 0),    // sizeLimit
		encodeInt(tagInteger, 0),    // timeLimit
		encode(tagBoolean, []byte{0}),
		encode(tagFilterEquality, concat(encodeString(tagOctetString, "objectClass"), encodeString(tagOctetString, class))),
		encode(tagSequence, sel),
	))
	return encode(tagSequence, concat(encodeInt(tagInteger, id), req))
}

// readMessage reads one LDAPMessage from r.
func readMessage(r io.Reader) ([]byte, error) {
	head := make([]byte, 2, 6)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, fmt.Errorf("ldap: reading a reply: %w", err)
	}
	if head[0] != tagSequence {
		return nil, fmt.Errorf("ldap: a reply that is not a message (0x%x)", head[0])
	}
	if l := head[1]; l >= 0x80 {
		k := int(l & 0x7f)
		if k == 0 || k > 4 {
			return nil, fmt.Errorf("ldap: unsupported length form 0x%x", l)
		}
		head = head[:2+k]
		if _, err := io.ReadFull(r, head[2:]); err != nil {
			return nil, fmt.Errorf("ldap: reading a reply: %w", err)
		}
	}
	_, n, hlen, err := header(append(head, 0)) // header wants a byte past it
	if err != nil {
		return nil, err
	}
	msg := make([]byte, hlen+n)
	copy(msg, head)
	if _, err := io.ReadFull(r, msg[hlen:]); err != nil {
		return nil, fmt.Errorf("ldap: reading a reply: %w", err)
	}
	return msg, nil
}

// splitMessage is an LDAPMessage's id and protocol operation (any
// controls after it are ignored).
func splitMessage(msg []byte) (int64, element, error) {
	m, rest, err := next(msg)
	if err != nil {
		return 0, element{}, err
	}
	if m.tag != tagSequence || len(rest) != 0 {
		return 0, element{}, errors.New("ldap: malformed message")
	}
	parts, err := children(m.body)
	if err != nil {
		return 0, element{}, err
	}
	if len(parts) < 2 || parts[0].tag != tagInteger || (len(parts) == 3 && parts[2].tag != tagControls) || len(parts) > 3 {
		return 0, element{}, errors.New("ldap: malformed message")
	}
	id, err := intValue(parts[0])
	return id, parts[1], err
}

// parseEntry reads a SearchResultEntry's contents.
func parseEntry(body []byte) (Entry, error) {
	parts, err := children(body)
	if err != nil {
		return Entry{}, err
	}
	if len(parts) != 2 || parts[0].tag != tagOctetString || parts[1].tag != tagSequence {
		return Entry{}, errors.New("ldap: malformed entry")
	}
	e := Entry{DN: string(parts[0].body), Attrs: map[string][]string{}}
	attrs, err := children(parts[1].body)
	if err != nil {
		return Entry{}, err
	}
	for _, a := range attrs {
		f, err := children(a.body)
		if err != nil {
			return Entry{}, err
		}
		if a.tag != tagSequence || len(f) != 2 || f[0].tag != tagOctetString || f[1].tag != tagSet {
			return Entry{}, errors.New("ldap: malformed attribute")
		}
		vals, err := children(f[1].body)
		if err != nil {
			return Entry{}, err
		}
		name := strings.ToLower(string(f[0].body))
		for _, v := range vals {
			if v.tag != tagOctetString {
				return Entry{}, errors.New("ldap: malformed attribute value")
			}
			e.Attrs[name] = append(e.Attrs[name], string(v.body))
		}
	}
	return e, nil
}

// parseResult reads an LDAPResult: its code and diagnostic message.
func parseResult(body []byte) (int64, string, error) {
	parts, err := children(body)
	if err != nil {
		return 0, "", err
	}
	if len(parts) < 3 || parts[0].tag != tagEnumerated || parts[1].tag != tagOctetString || parts[2].tag != tagOctetString {
		return 0, "", errors.New("ldap: malformed result")
	}
	code, err := intValue(parts[0])
	return code, string(parts[2].body), err
}
