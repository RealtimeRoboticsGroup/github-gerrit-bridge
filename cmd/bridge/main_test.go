package main

import (
	"net/http/httptest"
	"testing"
)

func TestHealthzDetailsOnlyFromThisHost(t *testing.T) {
	for _, tc := range []struct {
		remote, xff string
		want        bool
	}{
		{"127.0.0.1:5000", "", true},
		{"[::1]:5000", "", true},
		{"127.0.0.1:5000", "203.0.113.9", false}, // through a local proxy
		{"203.0.113.9:5000", "", false},
	} {
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := fromLoopback(r); got != tc.want {
			t.Errorf("%s (forwarded for %q) = %v", tc.remote, tc.xff, got)
		}
	}
}
