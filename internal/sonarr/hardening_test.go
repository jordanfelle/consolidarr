package sonarr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestZonedAndMappedAddressesAreBlocked(t *testing.T) {
	for _, h := range []string{"fe80::1%eth0", "fe80::1", "::ffff:169.254.169.254", "169.254.169.254", "0.0.0.0", "ff02::1%lo"} {
		if err := refuseUnsafe("tcp", "["+h+"]:80", nil); err == nil && strings.Contains(h, ":") {
			t.Errorf("dial to %s must be refused", h)
		}
		if err := refuseUnsafe("tcp", h+":80", nil); err == nil && !strings.Contains(h, ":") {
			t.Errorf("dial to %s must be refused", h)
		}
	}
	for _, u := range []string{"http://[fe80::1%25eth0]:8989", "http://[::ffff:169.254.169.254]/"} {
		if err := ValidateURL(u); err == nil {
			t.Errorf("%s must be rejected", u)
		}
	}
	// an unparseable destination at dial time fails closed
	if err := refuseUnsafe("tcp", "not-an-ip:80", nil); err == nil {
		t.Error("a non-literal dial address must be refused")
	}
}

func TestValidateURLRejectsQueryAndFragment(t *testing.T) {
	for _, u := range []string{"http://sonarr:8989/?x=1", "http://sonarr:8989/#frag", "http://sonarr:8989/?"} {
		if ValidateURL(u) == nil {
			t.Errorf("%s must be rejected", u)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = r.Header.Get("X-Api-Key") != "" }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer srv.Close()
	if _, err := New(srv.URL, "secret").Status(context.Background()); err == nil {
		t.Fatal("a redirect must be an error")
	}
	if leaked {
		t.Fatal("the API key was sent to the redirect target")
	}
}

func TestOversizedResponseIsRefused(t *testing.T) {
	old := maxResponse
	maxResponse = 1024
	defer func() { maxResponse = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"` + strings.Repeat("a", 4096) + `"}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "k").Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("want a size error, got %v", err)
	}
}
