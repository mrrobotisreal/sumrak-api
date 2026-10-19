// syncd is Sumrak's backup-sync service (design §9): it accepts encrypted
// backup archives from the app over Tailscale, lists them, serves them back
// for restore, and applies the T20 ring-buffer retention rule. It never
// decrypts anything — archives are opaque blobs, and neither the passphrase
// nor any key ever reaches this process.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mrrobotisreal/sumrak-api/internal/config"
	"github.com/mrrobotisreal/sumrak-api/internal/db"
	"github.com/mrrobotisreal/sumrak-api/internal/firebaseauth"
	"github.com/mrrobotisreal/sumrak-api/internal/server"
	"github.com/mrrobotisreal/sumrak-api/internal/storage"
	syncapi "github.com/mrrobotisreal/sumrak-api/internal/sync"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.FromEnv()
	if err != nil {
		log.Error("bad configuration", "err", err)
		os.Exit(1)
	}
	store, err := storage.New(cfg.DataDir, cfg.RetentionDailyDays)
	if err != nil {
		log.Error("storage init failed", "err", err)
		os.Exit(1)
	}

	opts := server.Options{Token: cfg.Token, MaxBodyBytes: cfg.MaxBodyBytes, Log: log}
	if cfg.SyncEnabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		pool, err := db.Open(ctx, cfg.DatabaseURL)
		if err == nil {
			var applied []string
			applied, err = db.Migrate(ctx, pool)
			log.Info("migrations", "applied", applied)
		}
		cancel()
		if err != nil {
			log.Error("database init failed", "err", err)
			os.Exit(1)
		}
		defer pool.Close()
		verifier := firebaseauth.New(cfg.FirebaseProjectID, firebaseauth.CertsURLFromEnv())
		opts.Sync = firebaseauth.Middleware(verifier, syncapi.New(pool, cfg.MaxPushBytes, log).Routes())
	} else {
		log.Info("/v1 sync API disabled (SYNCD_FIREBASE_PROJECT_ID unset)")
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(store, opts),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("syncd listening",
			"addr", cfg.Addr,
			"dataDir", cfg.DataDir,
			"retentionDays", cfg.RetentionDailyDays,
			"maxBodyBytes", cfg.MaxBodyBytes,
			"syncEnabled", cfg.SyncEnabled(),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("shutdown incomplete", "err", err)
	}
}
