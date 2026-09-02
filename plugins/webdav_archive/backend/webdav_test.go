package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseWebDAVBaseURLRefusesWhatCannotBeArchivedTo(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"empty", "   "},
		{"no scheme", "cloud.example.org/dav/"},
		{"file scheme", "file:///etc/passwd"},
		{"no host", "https:///dav/"},
		{"credentials in the address", "https://me:secret@cloud.example.org/dav/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseWebDAVBaseURL(tc.raw); err == nil {
				t.Fatalf("parseWebDAVBaseURL(%q) accepted an address it should refuse", tc.raw)
			}
		})
	}
}

func TestParseWebDAVBaseURLMakesTheBaseACollection(t *testing.T) {
	parsed, err := parseWebDAVBaseURL("https://cloud.example.org/dav/files/me?v=1#top")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/dav/files/me/" {
		t.Fatalf("path = %q, want a trailing slash so relative paths resolve under it", parsed.Path)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatalf("query/fragment survived: %q %q", parsed.RawQuery, parsed.Fragment)
	}
}

// A path arriving from a browser must never address anything above the folder
// the target was configured with, however it is spelled.
func TestResolveConfinesEveryPathToTheConfiguredBase(t *testing.T) {
	client, err := newWebDAVClient("https://cloud.example.org/dav/files/me/Recordings/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"../../../etc/passwd",
		"/../../secrets/",
		"a/../../../b.txt",
		"..",
		"./../x",
	} {
		resolved, err := client.resolve(raw)
		if err != nil {
			t.Fatalf("resolve(%q): %v", raw, err)
		}
		if !strings.HasPrefix(resolved.Path, "/dav/files/me/Recordings/") {
			t.Fatalf("resolve(%q) = %q, which is outside the configured base", raw, resolved.Path)
		}
	}
}

// The assertions here are on String(), not on Path. Path is the *decoded*
// path, so a check against it passes just as happily when the escaping is
// applied twice -- which is exactly the bug this guards: pre-escaped text in
// Path alone leaves as `%2520` and the file lands under a name with a literal
// percent in it.
func TestResolveEscapesEachSegmentExactlyOnce(t *testing.T) {
	client, err := newWebDAVClient("https://cloud.example.org/dav/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ raw, want string }{
		{"2026/05/voice memo.m4a", "https://cloud.example.org/dav/2026/05/voice%20memo.m4a"},
		{"2026/05/Sprachmemo Ü.m4a", "https://cloud.example.org/dav/2026/05/Sprachmemo%20%C3%9C.m4a"},
		{"a/b#1.m4a", "https://cloud.example.org/dav/a/b%231.m4a"},
		{"a/b?x.m4a", "https://cloud.example.org/dav/a/b%3Fx.m4a"},
		{"2026/05/", "https://cloud.example.org/dav/2026/05/"},
	} {
		resolved, err := client.resolve(tc.raw)
		if err != nil {
			t.Fatalf("resolve(%q): %v", tc.raw, err)
		}
		if got := resolved.String(); got != tc.want {
			t.Errorf("resolve(%q).String() = %q, want %q", tc.raw, got, tc.want)
		}
		// Path stays the decoded form, which is what makes the double escape
		// detectable at all.
		if strings.Contains(resolved.Path, "%") {
			t.Errorf("resolve(%q).Path = %q, want the decoded path", tc.raw, resolved.Path)
		}
	}
}

// The end the bug actually broke: a file with a space in its name has to be
// written, listed and read back under the same name.
func TestASpacedNameSurvivesPutListAndGet(t *testing.T) {
	const name = "voice memo.m4a"
	stored := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server sees the wire form and decodes it exactly once, the way
		// any real WebDAV server does.
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil {
			t.Errorf("server could not decode %q: %v", r.URL.EscapedPath(), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			stored[decoded] = body
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			body, ok := stored[decoded]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "audio/mp4")
			_, _ = w.Write(body)
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response><d:href>/dav/%s</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status>
      <d:prop><d:resourcetype/><d:getcontentlength>5</d:getcontentlength></d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`, url.PathEscape(name))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client, err := newWebDAVClient(server.URL+"/dav/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := client.Put(ctx, name, []byte("bytes"), "audio/mp4"); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored["/dav/"+name]; !ok {
		t.Fatalf("stored under %v, want the plain name /dav/%s", keysOf(stored), name)
	}

	entries, err := client.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != name {
		t.Fatalf("listing = %+v, want one entry named %q", entries, name)
	}

	// The path the browser is handed back by the listing has to be one the
	// download route can fetch again.
	body, _, _, err := client.Get(ctx, entries[0].Path)
	if err != nil {
		t.Fatalf("get %q: %v", entries[0].Path, err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "bytes" {
		t.Fatalf("round-tripped body = %q", got)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

const listingBody = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/dav/</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/2026/</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/memo%20one.m4a</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status>
      <d:prop>
        <d:resourcetype/>
        <d:getcontentlength>2048</d:getcontentlength>
        <d:getcontenttype>audio/mp4</d:getcontenttype>
        <d:getlastmodified>Tue, 05 May 2026 10:00:00 GMT</d:getlastmodified>
      </d:prop>
    </d:propstat>
    <d:propstat><d:status>HTTP/1.1 404 Not Found</d:status>
      <d:prop><d:getcontentlanguage/></d:prop></d:propstat>
  </d:response>
</d:multistatus>`

func TestListReadsPropfindAndDropsTheCollectionItself(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			t.Errorf("method = %s, want PROPFIND", r.Method)
		}
		if r.Header.Get("Depth") != "1" {
			t.Errorf("Depth = %q, want 1", r.Header.Get("Depth"))
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(listingBody))
	}))
	defer server.Close()

	client, err := newWebDAVClient(server.URL+"/dav/", "user", "secret")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := client.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d (%+v), want the folder and the file but not the collection itself", len(entries), entries)
	}
	if !entries[0].IsDir || entries[0].Name != "2026" || entries[0].Path != "2026/" {
		t.Fatalf("folder entry = %+v", entries[0])
	}
	file := entries[1]
	if file.IsDir || file.Name != "memo one.m4a" {
		t.Fatalf("file entry = %+v, want the href percent-decoded", file)
	}
	if file.Size != 2048 || file.ContentType != "audio/mp4" || file.ModifiedAt == 0 {
		t.Fatalf("file properties = %+v, want them read from the 200 propstat only", file)
	}
}

func TestPutCreatesMissingCollectionsAndRetries(t *testing.T) {
	var methods []string
	var putCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodPut:
			putCount++
			if putCount == 1 {
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client, err := newWebDAVClient(server.URL+"/dav/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Put(context.Background(), "2026/05/memo.m4a", []byte("bytes"), "audio/mp4"); err != nil {
		t.Fatal(err)
	}
	if putCount != 2 {
		t.Fatalf("PUT count = %d, want a retry after the collections were made", putCount)
	}
	joined := strings.Join(methods, " | ")
	if !strings.Contains(joined, "MKCOL /dav/2026/") || !strings.Contains(joined, "MKCOL /dav/2026/05/") {
		t.Fatalf("methods = %s, want a MKCOL per missing level", joined)
	}
}

// A collection that is already there answers 405, which is a success for a
// MKCOL run before every upload rather than only on a miss.
func TestMakeCollectionsTreatsAnExistingCollectionAsDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer server.Close()
	client, err := newWebDAVClient(server.URL+"/dav/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.MakeCollections(context.Background(), "2026/05"); err != nil {
		t.Fatalf("MakeCollections on an existing tree: %v", err)
	}
}

func TestClientRefusesRedirectsToAnotherHost(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the client followed a redirect to a host the guard never saw")
	}))
	defer elsewhere.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/dav/", http.StatusFound)
	}))
	defer server.Close()

	client, err := newWebDAVClient(server.URL+"/dav/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(context.Background(), ""); err == nil {
		t.Fatal("a cross-host redirect was followed")
	}
}

func TestExistsAndDeleteReadTheServersAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/there.m4a") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client, err := newWebDAVClient(server.URL+"/dav/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ok, err := client.Exists(ctx, "there.m4a"); err != nil || !ok {
		t.Fatalf("Exists(there) = %v, %v", ok, err)
	}
	if ok, err := client.Exists(ctx, "gone.m4a"); err != nil || ok {
		t.Fatalf("Exists(gone) = %v, %v", ok, err)
	}
	if err := client.Delete(ctx, "gone.m4a"); err != errNotFound {
		t.Fatalf("Delete(gone) = %v, want errNotFound", err)
	}
}

// The metadata endpoint is the address an SSRF is worth attempting, and it
// stays refused whether or not the operator allows private hosts.
func TestBlockedWebDAVIPAlwaysRefusesLinkLocal(t *testing.T) {
	for _, allowPrivate := range []bool{true, false} {
		if !blockedWebDAVIP(net.ParseIP("169.254.169.254"), allowPrivate) {
			t.Fatalf("cloud metadata address allowed with allowPrivate=%v", allowPrivate)
		}
		if !blockedWebDAVIP(net.ParseIP("fe80::1"), allowPrivate) {
			t.Fatalf("IPv6 link-local allowed with allowPrivate=%v", allowPrivate)
		}
		if !blockedWebDAVIP(net.ParseIP("::ffff:169.254.169.254"), allowPrivate) {
			t.Fatalf("IPv4-mapped metadata address allowed with allowPrivate=%v", allowPrivate)
		}
	}
}

// A self-hosted WebDAV is normally on the same private network, so the default
// has to allow it -- and the strict setting has to take it away.
func TestBlockedWebDAVIPFollowsThePrivateHostSetting(t *testing.T) {
	for _, address := range []string{"192.168.1.10", "10.0.0.5", "127.0.0.1", "fd00::1"} {
		ip := net.ParseIP(address)
		if blockedWebDAVIP(ip, true) {
			t.Fatalf("%s refused by default, which blocks the self-hosted case", address)
		}
		if !blockedWebDAVIP(ip, false) {
			t.Fatalf("%s allowed with private hosts turned off", address)
		}
	}
	if blockedWebDAVIP(net.ParseIP("203.0.113.9"), true) == false {
		t.Fatal("a documentation range was dialed")
	}
	if blockedWebDAVIP(net.ParseIP("93.184.216.34"), true) {
		t.Fatal("an ordinary public address was refused")
	}
}

func TestPrivateWebDAVHostsAllowedReadsTheOptOut(t *testing.T) {
	t.Setenv("ROLLTOP_WEBDAV_ALLOW_PRIVATE_HOSTS", "0")
	if privateWebDAVHostsAllowed() {
		t.Fatal("the opt-out was ignored")
	}
	t.Setenv("ROLLTOP_WEBDAV_ALLOW_PRIVATE_HOSTS", "")
	if !privateWebDAVHostsAllowed() {
		t.Fatal("private hosts should be allowed unless the operator says otherwise")
	}
}

func TestCleanRemotePathKeepsCollectionsMarked(t *testing.T) {
	for raw, want := range map[string]string{
		"":             "",
		"/":            "",
		"a/b":          "a/b",
		"/a/b/":        "a/b/",
		"a//b":         "a/b",
		"a/../b":       "b",
		"../../x":      "x",
		"a\\b":         "a/b",
		"  /a/b.txt  ": "a/b.txt",
	} {
		if got := cleanRemotePath(raw); got != want {
			t.Errorf("cleanRemotePath(%q) = %q, want %q", raw, got, want)
		}
	}
}
