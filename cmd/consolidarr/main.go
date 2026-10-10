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
	go eng.Start(ctx)

	a := &api.API{St: st, Eng: eng, Web: web.FS(), Version: version,
		AuthDisabled: os.Getenv("CONSOLIDARR_AUTH_DISABLED") == "true"}
	srv := &http.Server{Addr: *addr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = srv.Shutdown(sh)
	}()
	log.Printf("consolidarr %s listening on %s", version, *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
