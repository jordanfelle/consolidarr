package sonarr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestValidateURL(t *testing.T) {
	good := []string{
		"http://sonarr:8989/sonarr",
		"https://sonarr.example.com",
		"http://10.0.0.35:8989/sonarr",
		"http://127.0.0.1:8989",
		"http://localhost:8989/sonarr4k",
		"http://[::1]:8989",
		"http://definitely-not-resolvable.invalid:8989", // lookup failure is not an error: Sonarr may just be down
	}
	for _, u := range good {
		if err := ValidateURL(u); err != nil {
			t.Errorf("%s should be allowed: %v", u, err)
		}
	}
	bad := map[string]string{
		"ftp://host":               "scheme",
		"file:///etc/passwd":       "scheme",
		"gopher://host":            "scheme",
		"":                         "empty",
		"//host":                   "no scheme",
		"http://":                  "no host",
		"http://user:pw@host:8989": "credentials",
		"http://169.254.169.254/latest/meta-data": "cloud metadata (link-local)",
		"http://169.254.1.1":                      "link-local",
		"http://[fe80::1]:8989":                   "ipv6 link-local",
		"http://0.0.0.0:8989":                     "unspecified",
		"http://[::]:8989":                        "ipv6 unspecified",
		"http://224.0.0.1":                        "multicast",
	}
	for u, why := range bad {
		if err := ValidateURL(u); err == nil {
			t.Errorf("%s (%s) must be rejected", u, why)
		}
	}
}

func TestClientRefusesUnsafeAddressesAtDialTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Bypass ValidateURL on purpose: the dial-time guard is what stops a host name that resolves, or later
	// re-resolves, to an unsafe address.
	for _, base := range []string{"http://169.254.169.254", "http://[fe80::1]:8989", "http://0.0.0.0:8989"} {
		_, err := New(base, "k").Status(ctx)
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s: want ErrBlockedAddress, got %v", base, err)
		}
	}
}

func TestClientStillReachesLoopbackAndPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" || r.URL.Path != "/sonarr/api/v3/system/status" {
			http.Error(w, "unexpected request "+r.URL.Path, 400)
			return
		}
		_, _ = w.Write([]byte(`{"version":"4.0.20"}`))
	}))
	defer srv.Close() // listens on 127.0.0.1: loopback must keep working
	v, err := New(srv.URL+"/sonarr", "k").Status(context.Background())
	if err != nil || v != "4.0.20" {
		t.Fatalf("loopback Sonarr must work: v=%q err=%v", v, err)
	}
}

func TestQueueFollowsEveryPage(t *testing.T) {
	const total = 1234
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
		var recs []map[string]int
		for i := (page - 1) * size; i < page*size && i < total; i++ {
			recs = append(recs, map[string]int{"id": i + 1})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"totalRecords": total, "records": recs})
	}))
	defer srv.Close()
	q, err := New(srv.URL, "k").Queue(context.Background(), time.Minute)
	if err != nil || len(q) != total {
		t.Fatalf("want all %d queue items across pages, got %d (err=%v)", total, len(q), err)
	}
	if pages != 3 {
		t.Fatalf("1234 items at 500 per page is 3 requests, got %d", pages)
	}
}
