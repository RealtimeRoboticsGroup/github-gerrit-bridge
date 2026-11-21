package ldap

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testServer is an in-process LDAPv3 server over TLS: it answers a
// subtree search with the entries whose objectClass is the filter's,
// under the base, and with result code code (success if 0).
type testServer struct {
	t        *testing.T
	ln       net.Listener
	caFile   string
	mu       sync.Mutex
	entries  []testEntry
	code     int64
	refer    bool  // send a referral too
	bindCode int64 // the bind's result code
	reqs     [][]byte
}

type testEntry struct {
	dn      string
	classes []string
	attrs   map[string][]string // as the server would name them
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ldap.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"ldap.test"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{t: t, ln: ln, caFile: caFile}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *testServer) url() string { return "ldaps://" + s.ln.Addr().String() }

func (s *testServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *testServer) handle(c net.Conn) {
	defer c.Close()
	for {
		msg, err := readMessage(c)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, msg)
		entries, code := s.entries, s.code
		s.mu.Unlock()
		id, op, err := splitMessage(msg)
		if err != nil {
			return
		}
		switch op.tag {
		case 0x60: // BindRequest
			s.mu.Lock()
			bc := s.bindCode
			s.mu.Unlock()
			c.Write(encode(tagSequence, concat(encodeInt(tagInteger, id),
				encode(0x61, concat(encodeInt(tagEnumerated, bc), encodeString(tagOctetString, ""), encodeString(tagOctetString, ""))))))
		case tagSearchRequest:
			s.search(c, id, op, entries, code)
		default: // an unbind, or anything else
			return
		}
	}
}

func (s *testServer) search(c net.Conn, id int64, op element, entries []testEntry, code int64) {
	parts, err := children(op.body)
	if err != nil || len(parts) != 8 {
		return
	}
	base := string(parts[0].body)
	f, _ := children(parts[6].body)
	class := string(f[1].body)
	for _, e := range entries {
		if !strings.HasSuffix(strings.ToLower(e.dn), strings.ToLower(base)) || !containsFold(e.classes, class) {
			continue
		}
		var attrs []byte
		for name, vals := range e.attrs {
			var set []byte
			for _, v := range vals {
				set = append(set, encodeString(tagOctetString, v)...)
			}
			attrs = append(attrs, encode(tagSequence, concat(encodeString(tagOctetString, name), encode(tagSet, set)))...)
		}
		c.Write(encode(tagSequence, concat(encodeInt(tagInteger, id),
			encode(tagSearchEntry, concat(encodeString(tagOctetString, e.dn), encode(tagSequence, attrs))))))
	}
	s.mu.Lock()
	refer := s.refer
	s.mu.Unlock()
	if refer {
		c.Write(encode(tagSequence, concat(encodeInt(tagInteger, id), encode(tagSearchReference, encodeString(tagOctetString, "ldaps://elsewhere/")))))
	}
	c.Write(encode(tagSequence, concat(encodeInt(tagInteger, id),
		encode(tagSearchDone, concat(encodeInt(tagEnumerated, code), encodeString(tagOctetString, ""), encodeString(tagOctetString, ""))))))
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func (s *testServer) set(code int64, entries ...testEntry) {
	s.mu.Lock()
	s.entries, s.code = entries, code
	s.mu.Unlock()
}

// person is an entry of ou=People with a GitHub account.
func person(uid, login, id string) testEntry {
	return testEntry{dn: "uid=" + uid + ",ou=People,dc=spacecookies,dc=dev", classes: []string{"inetOrgPerson", "githubAccount"},
		attrs: map[string][]string{"uid": {uid}, "githubLogin": {login}, "githubId": {id}}}
}

func (s *testServer) server(t *testing.T) *Server {
	t.Helper()
	srv, err := ParseServer(s.url(), s.caFile)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// A search over TLS, trusting the CA file, returns the entries of the
// class under the base; the request is the one RFC 4511 describes.
func TestSearch(t *testing.T) {
	ts := newTestServer(t)
	other := person("x", "x", "1")
	other.dn = "uid=x,ou=Other,dc=spacecookies,dc=dev"
	plain := person("y", "y", "2")
	plain.classes = []string{"inetOrgPerson"}
	ts.set(0, person("austin", "AustinSchuh", "294043"), other, plain)
	got, err := ts.server(t).Search(context.Background(), "ou=People,dc=spacecookies,dc=dev", "githubAccount", []string{"uid", "githubLogin", "githubId"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Attrs["githublogin"][0] != "AustinSchuh" || got[0].Attrs["githubid"][0] != "294043" || got[0].Attrs["uid"][0] != "austin" {
		t.Errorf("entries %+v", got)
	}
	// The request: SEQUENCE { 1, SearchRequest { base, wholeSubtree,
	// neverDeref, 0, 0, FALSE, (objectClass=githubAccount), attrs } }.
	// An anonymous bind first, then the search.
	if !bytes.Equal(ts.reqs[0], bindRequest(1)) {
		t.Errorf("bind % x", ts.reqs[0])
	}
	want := searchRequest(2, "ou=People,dc=spacecookies,dc=dev", "githubAccount", []string{"uid", "githubLogin", "githubId"})
	if len(ts.reqs) < 2 || !bytes.Equal(ts.reqs[1], want) {
		t.Errorf("requests % x", ts.reqs)
	}
}

// What a search will not take: a server it cannot verify, a result other
// than success (a size limit, say), a server that is not there.
func TestSearchFails(t *testing.T) {
	ts := newTestServer(t)
	ts.set(0, person("austin", "AustinSchuh", "294043"))
	ctx := context.Background()
	untrusted := ts.server(t)
	untrusted.RootCAs = x509.NewCertPool()
	if _, err := untrusted.Search(ctx, "dc=dev", "githubAccount", nil); err == nil {
		t.Error("searched a server whose certificate is not trusted")
	}
	wrongName := ts.server(t)
	wrongName.ServerName = "ldap.elsewhere"
	if _, err := wrongName.Search(ctx, "dc=dev", "githubAccount", nil); err == nil {
		t.Error("searched a server whose certificate is for another name")
	}
	ts.set(4, person("austin", "AustinSchuh", "294043"))
	var re *ResultError
	if _, err := ts.server(t).Search(ctx, "dc=dev", "githubAccount", nil); !errors.As(err, &re) || re.Code != 4 || !strings.Contains(err.Error(), "size limit") {
		t.Errorf("size limit: %v", err)
	}
	// A referral: part of the answer is elsewhere, so this one is not
	// whole.
	ts.set(0, person("austin", "AustinSchuh", "294043"))
	ts.mu.Lock()
	ts.refer = true
	ts.mu.Unlock()
	if _, err := ts.server(t).Search(ctx, "dc=dev", "githubAccount", nil); err == nil || !strings.Contains(err.Error(), "referral") {
		t.Errorf("referral: %v", err)
	}
	srv := ts.server(t)
	ts.ln.Close()
	if _, err := srv.Search(ctx, "dc=dev", "githubAccount", nil); err == nil {
		t.Error("searched a server that is down")
	}
}

// Only ldaps://host[:port], and a CA file with certificates in it.
func TestParseServer(t *testing.T) {
	for _, bad := range []string{"ldap://ldap.spacecookies.dev", "ldaps://", "ldaps://u:p@host", "ldaps://host/dc=x", "ldaps://host?x", "https://host"} {
		if _, err := ParseServer(bad, ""); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	s, err := ParseServer("ldaps://ldap.spacecookies.dev", "")
	if err != nil || s.Addr != "ldap.spacecookies.dev:636" || s.ServerName != "ldap.spacecookies.dev" || s.RootCAs != nil {
		t.Errorf("%+v %v", s, err)
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(empty, []byte("nothing"), 0o600)
	if _, err := ParseServer("ldaps://host", empty); err == nil {
		t.Error("a CA file with no certificates accepted")
	}
}

// fakeSearcher answers with entries, or err.
type fakeSearcher struct {
	entries []Entry
	err     error
}

func (f *fakeSearcher) Search(context.Context, string, string, []string) ([]Entry, error) {
	return f.entries, f.err
}

func entry(uid, login, id string) Entry {
	return Entry{DN: "uid=" + uid + ",ou=People", Attrs: map[string][]string{"uid": {uid}, "githublogin": {login}, "githubid": {id}}}
}

// What a read leaves out: entries that are not valid, and every entry of
// a clash, by GitHub id, login (any case) or Gerrit user.
func TestDirectoryRefusesWhatItCannotTrust(t *testing.T) {
	two := entry("multi", "multi", "7")
	two.Attrs["githubid"] = []string{"7", "8"}
	f := &fakeSearcher{entries: []Entry{
		entry("austin", "AustinSchuh", "294043"),
		entry("dupid1", "one", "5"), entry("dupid2", "two", "5"), // one GitHub id, two users
		entry("duplogin1", "Same", "11"), entry("duplogin2", "same", "12"), // one login (by case)
		entry("dupuser", "a1", "21"), entry("DupUser", "a2", "22"), // one user (by case)
		entry("longlogin", strings.Repeat("a", 40), "31"),
		entry("badlogin", "-bad", "32"), entry("badlogin2", "a--b", "33"),
		entry("badid", "badid", "x34"), entry("zeroid", "zeroid", "0"), entry("leadzero", "leadzero", "035"),
		two,
		{DN: "uid=nologin", Attrs: map[string][]string{"uid": {"nologin"}, "githubid": {"41"}}},
		entry("ok39", strings.Repeat("b", 39), "42"),
	}}
	d := &Directory{Searcher: f, BaseDN: "ou=People", ObjectClass: "githubAccount", Refresh: time.Minute}
	if err := d.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{294043: "austin", 42: "ok39"} {
		if a, found, err := d.ByGitHubID(id); err != nil || !found || a.GerritUser != want {
			t.Errorf("id %d: %+v %v %v, want %s", id, a, found, err, want)
		}
	}
	for _, id := range []int64{5, 11, 12, 21, 22, 31, 32, 33, 7, 8, 41} {
		if a, found, _ := d.ByGitHubID(id); found {
			t.Errorf("id %d left in: %+v", id, a)
		}
	}
	for _, user := range []string{"dupid1", "dupid2", "duplogin1", "dupuser", "multi", "badid"} {
		if _, found, _ := d.ByGerritUser(user); found {
			t.Errorf("user %s left in", user)
		}
	}
	if a, found, _ := d.ByGerritUser("AUSTIN"); !found || a.GitHubID != 294043 {
		t.Errorf("by user, any case: %+v %v", a, found)
	}
}

// A changed id moves the account; a removed entry stops matching at the
// next read; a read that fails, or none for two intervals, answers
// nothing.
func TestDirectoryFollowsTheDirectory(t *testing.T) {
	ctx := context.Background()
	f := &fakeSearcher{entries: []Entry{entry("austin", "AustinSchuh", "294043")}}
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	d := &Directory{Searcher: f, BaseDN: "ou=People", ObjectClass: "githubAccount", Refresh: 5 * time.Minute, now: func() time.Time { return now }}
	if _, _, err := d.ByGitHubID(294043); !errors.Is(err, ErrNoDirectory) {
		t.Errorf("before any read: %v", err)
	}
	d.Read(ctx)
	f.entries = []Entry{entry("austin", "AustinSchuh2", "999")}
	d.Read(ctx)
	if _, found, _ := d.ByGitHubID(294043); found {
		t.Error("the old id still matches")
	}
	if a, found, _ := d.ByGitHubID(999); !found || a.GerritUser != "austin" {
		t.Errorf("the new id: %+v %v", a, found)
	}
	f.entries = nil
	d.Read(ctx)
	if _, found, err := d.ByGitHubID(999); found || err != nil {
		t.Errorf("removed: %v %v", found, err)
	}
	f.entries = []Entry{entry("austin", "AustinSchuh", "294043")}
	d.Read(ctx)
	f.err = errors.New("ldap down")
	if d.Read(ctx) == nil {
		t.Fatal("no error")
	}
	if _, found, err := d.ByGitHubID(294043); found || !errors.Is(err, ErrNoDirectory) || !strings.Contains(err.Error(), "ldap down") {
		t.Errorf("after a failed read: %v %v", found, err)
	}
	f.err = nil
	d.Read(ctx)
	now = now.Add(11 * time.Minute)
	if _, _, err := d.ByGitHubID(294043); !errors.Is(err, ErrNoDirectory) {
		t.Errorf("read two intervals ago: %v", err)
	}
}

// The Directory reads through the TLS client as it would in production.
func TestDirectoryOverTLS(t *testing.T) {
	ts := newTestServer(t)
	ts.set(0, person("austin", "AustinSchuh", "294043"), person("james", "jamesonGH", "1234"))
	d := &Directory{Searcher: ts.server(t), BaseDN: "ou=People,dc=spacecookies,dc=dev", ObjectClass: "githubAccount", Refresh: time.Minute,
		Logger: slog.New(slog.DiscardHandler)}
	if err := d.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a, found, _ := d.ByGitHubID(1234); !found || a.GerritUser != "james" || a.GitHubLogin != "jamesonGH" {
		t.Errorf("%+v %v", a, found)
	}
}

// A real client, OpenLDAP's ldapsearch, reads the test server's replies,
// and the server (with the client's own parsing) reads its request: the
// two do not share a mistake in BER. Skipped where ldapsearch is not.
func TestAgainstLDAPSearch(t *testing.T) {
	bin, err := exec.LookPath("ldapsearch")
	if err != nil {
		t.Skip("no ldapsearch")
	}
	ts := newTestServer(t)
	ts.set(0, person("austin", "AustinSchuh", "294043"))
	cmd := exec.Command(bin, "-x", "-LLL", "-H", ts.url(), "-b", "ou=People,dc=spacecookies,dc=dev", "(objectClass=githubAccount)", "uid", "githubLogin", "githubId")
	cmd.Env = append(os.Environ(), "LDAPTLS_CACERT="+ts.caFile, "LDAPTLS_REQCERT=demand", "LDAPCONF=/dev/null", "LDAPRC=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ldapsearch: %v\n%s", err, out)
	}
	for _, want := range []string{"dn: uid=austin,ou=People,dc=spacecookies,dc=dev", "uid: austin", "githubLogin: AustinSchuh", "githubId: 294043"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("ldapsearch printed %q, without %q", out, want)
		}
	}
	// Its request, as the server read it: a subtree search of the base
	// for objectClass=githubAccount, like the client's own.
	var op element
	for _, r := range ts.reqs {
		if _, o, err := splitMessage(r); err == nil && o.tag == tagSearchRequest {
			op = o
		}
	}
	parts, _ := children(op.body)
	scope, _ := intValue(parts[1])
	f, _ := children(parts[6].body)
	if op.tag != tagSearchRequest || string(parts[0].body) != "ou=People,dc=spacecookies,dc=dev" || scope != 2 ||
		parts[6].tag != tagFilterEquality || string(f[0].body) != "objectClass" || string(f[1].body) != "githubAccount" {
		t.Errorf("ldapsearch's request % x", op.body)
	}
	mine := searchRequest(1, "ou=People,dc=spacecookies,dc=dev", "githubAccount", nil)
	_, myOp, _ := splitMessage(mine)
	myParts, _ := children(myOp.body)
	if !bytes.Equal(myParts[6].body, parts[6].body) || !bytes.Equal(myParts[0].body, parts[0].body) || !bytes.Equal(myParts[1].body, parts[1].body) {
		t.Errorf("the bridge's filter or scope differs from ldapsearch's: % x vs % x", myOp.body, op.body)
	}
}

// A refused anonymous bind is an error, not an empty directory; a
// four-byte length (as some servers write) is read.
func TestBindRefusedAndLongLengths(t *testing.T) {
	ts := newTestServer(t)
	ts.set(0, person("austin", "AustinSchuh", "294043"))
	ts.mu.Lock()
	ts.bindCode = 48 // inappropriateAuthentication
	ts.mu.Unlock()
	if _, err := ts.server(t).Search(context.Background(), "dc=dev", "githubAccount", nil); err == nil || !strings.Contains(err.Error(), "bind refused") {
		t.Errorf("refused bind: %v", err)
	}
	b := []byte{0x30, 0x84, 0, 0, 0, 3, 0x02, 0x01, 0x05}
	msg, err := readMessage(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if e, _, err := next(msg); err != nil || e.tag != tagSequence || len(e.body) != 3 {
		t.Errorf("% x: %+v %v", msg, e, err)
	}
}

// A clash counts every entry, valid or not: an invalid second entry for
// someone's id or uid leaves out the valid one too.
func TestInvalidEntryStillClashes(t *testing.T) {
	bad := entry("alice", "bad_login", "100") // invalid login, same id and uid
	d := &Directory{Searcher: &fakeSearcher{entries: []Entry{entry("alice", "aliceGH", "100"), bad, entry("bob", "bobGH", "200")}},
		BaseDN: "ou=People", ObjectClass: "githubAccount", Refresh: time.Minute}
	d.Read(context.Background())
	if a, found, _ := d.ByGitHubID(100); found {
		t.Errorf("alice still matched: %+v", a)
	}
	if _, found, _ := d.ByGitHubID(200); !found {
		t.Error("bob, in no clash, not matched")
	}
}
