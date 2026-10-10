// consolidarr turns Sonarr seasons assembled from single-episode grabs into season packs.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
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

	if err := os.MkdirAll(*data, 0o755); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(*data, *dburl)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	eng := engine.New(st)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	engDone := make(chan struct{})
	go func() { defer close(engDone); eng.Start(ctx) }()

	a := &api.API{St: st, Eng: eng, Web: web.FS(), Version: version,
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
		sh, c := context.WithTimeout(context.Background(), 10*time.Second)
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
	select {
	case <-engDone:
	case <-time.After(15 * time.Second):
		log.Print("engine did not stop in time")
	}
}
