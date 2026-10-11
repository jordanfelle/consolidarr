// consolidarr turns Sonarr seasons assembled from single-episode grabs into season packs.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jordanfelle/consolidarr/internal/api"
	"github.com/jordanfelle/consolidarr/internal/engine"
	"github.com/jordanfelle/consolidarr/internal/store"
	"github.com/jordanfelle/consolidarr/web"
)

var version = "dev"

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	addr := flag.String("listen", env("CONSOLIDARR_LISTEN", ":7480"), "listen address")
	data := flag.String("data", env("CONSOLIDARR_DATA", "/data"), "data directory (SQLite lives here)")
	dburl := flag.String("database-url", env("CONSOLIDARR_DATABASE_URL", ""), "postgres://... (default: SQLite in the data dir)")
	flag.Parse()

	// Read the restriction before anything starts: a typo must stop the container, not run it open (or leave work half-started).
	ipAllow, err := api.ParseIPAllow(os.Getenv("CONSOLIDARR_ALLOWED_IPS"), os.Getenv("CONSOLIDARR_TRUSTED_PROXIES"))
	if err != nil {
		log.Fatal(err) // a restriction that was asked for but cannot be read must not silently become no restriction
	}
	if err := os.MkdirAll(*data, 0o755); err != nil {
		log.Fatal(err)
	}
	st, err := openWithRetry(*data, *dburl, 2*time.Minute)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	eng := engine.New(st)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	engDone := make(chan struct{})
	go func() { defer close(engDone); eng.Start(ctx) }()

	a := &api.API{St: st, IPAllow: ipAllow, Eng: eng, Web: web.FS(), Version: version,
		AuthDisabled:   os.Getenv("CONSOLIDARR_AUTH_DISABLED") == "true",
		CookieSecure:   os.Getenv("CONSOLIDARR_COOKIE_SECURE"),
		AllowedHosts:   api.ParseAllowedHosts(os.Getenv("CONSOLIDARR_ALLOWED_HOSTS")),
		TrustedOrigins: api.ParseAllowedHosts(os.Getenv("CONSOLIDARR_TRUSTED_ORIGINS"))}
	if a.AuthDisabled {
		log.Print("login is DISABLED: put this behind an authenticating proxy (Cloudflare Access) or keep it on a trusted LAN; dotted host names are refused unless CONSOLIDARR_ALLOWED_HOSTS lists them")
	}
	srv := &http.Server{Addr: *addr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	shut := make(chan struct{})
	go func() {
		defer close(shut)
		<-ctx.Done()
		sh, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(sh)
	}()
	log.Printf("consolidarr %s listening on %s", version, *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	// ListenAndServe returns as soon as Shutdown starts; wait for in-flight requests and the engine so a
	// restart does not cut a replacement or a database write in half.
	<-shut
	eng.Drain()              // set synchronously: WaitIdle below must never run before the flag
	grace := 8 * time.Second // a courtesy, not a requirement: correctness comes from crash recovery, so keep this inside docker's default 10s stop timeout
	if d, err := time.ParseDuration(os.Getenv("CONSOLIDARR_SHUTDOWN_GRACE")); err == nil && d > 0 {
		grace = d
	}
	deadline := time.Now().Add(grace)
	select {
	case <-engDone:
	case <-time.After(grace):
		log.Print("engine did not stop in time")
	}
	if !eng.WaitIdle(time.Until(deadline)) {
		log.Print("a replacement was still between delete and import at shutdown; this is fine: the job resumes as 'importing' on the next start")
	}
}

// openWithRetry waits for a database that is still starting (or being restarted by an update): connection-type
// failures against a Postgres DSN are retried with backoff instead of crash-looping the container. Anything else (a
// bad DSN, a failing migration, SQLite) fails at once with the real error.
func openWithRetry(dir, url string, wait time.Duration) (*store.Store, error) {
	deadline := time.Now().Add(wait)
	delay := time.Second
	for {
		st, err := store.Open(dir, url)
		if err == nil || !strings.HasPrefix(url, "postgres") || !transient(err) || time.Now().Add(delay).After(deadline) {
			return st, err
		}
		log.Printf("database not ready (%v); retrying in %s", err, delay)
		time.Sleep(delay)
		delay = min(delay*2, 15*time.Second)
	}
}

// transient reports whether err looks like "the server is not accepting connections yet".
func transient(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	m := strings.ToLower(err.Error())
	for _, s := range []string{"connection refused", "no such host", "starting up", "shutting down", "connection reset", "eof", "timeout", "i/o", "network is unreachable", "recovery mode"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
