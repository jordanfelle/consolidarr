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
	"github.com/jordanfelle/consolidarr/internal/meta"
	"github.com/jordanfelle/consolidarr/internal/qbit"
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
	// qBittorrent through qui's client proxy (http://qui:7476/proxy/<key>), like Sonarr's own download client. The URL
	// holds a key, so it comes from the environment and never the settings API. Without it relabelled packs are not fetched.
	if u := strings.TrimSpace(os.Getenv("CONSOLIDARR_QBIT_URL")); u != "" {
		eng.Qbit = qbit.New(u, os.Getenv("CONSOLIDARR_QBIT_USER"), os.Getenv("CONSOLIDARR_QBIT_PASS"))
		log.Printf("qBittorrent access on (relabelled packs are parked and checked there)")
	}
	eng.Meta = metaFromEnv()
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

// metaFromEnv builds the optional TVDB/TMDB lookups of alternative episode orders. Keys come from the environment
// (never the settings API, which anyone on the LAN can read). With neither set, only Sonarr's own titles are used.
func metaFromEnv() *meta.Lookup {
	l := &meta.Lookup{}
	if k := strings.TrimSpace(os.Getenv("CONSOLIDARR_TVDB_KEY")); k != "" {
		l.TVDB = &meta.TVDB{Key: k, Pin: strings.TrimSpace(os.Getenv("CONSOLIDARR_TVDB_PIN"))}
		log.Printf("alternative episode orders: TVDB lookups on")
	}
	if k := strings.TrimSpace(os.Getenv("CONSOLIDARR_TMDB_KEY")); k != "" {
		l.TMDB = &meta.TMDB{Key: k}
		log.Printf("alternative episode orders: TMDB lookups on")
	}
	if !l.Configured() {
		return nil
	}
	return l
}
