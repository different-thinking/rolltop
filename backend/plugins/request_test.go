package plugins

import (
	"net/http"
	"testing"
)

func TestRequestIsHTTPSReadsForwardedProto(t *testing.T) {
	cases := []struct {
		name  string
		proto string
		want  bool
	}{
		{"plain", "http", false},
		{"https", "https", true},
		{"trailing space", "https ", true},
		{"leading space", " https", true},
		{"proxy chain client https", "https, http", true},
		{"proxy chain client http", "http, https", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptestRequest(tc.proto)
			if got := RequestIsHTTPS(r); got != tc.want {
				t.Fatalf("RequestIsHTTPS(X-Forwarded-Proto=%q) = %v, want %v", tc.proto, got, tc.want)
			}
		})
	}
}

func httptestRequest(forwardedProto string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://example.test/", nil)
	if forwardedProto != "" {
		r.Header.Set("X-Forwarded-Proto", forwardedProto)
	}
	return r
}
