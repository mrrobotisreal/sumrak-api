// Package server wires syncd's three endpoints (design §9 contract):
//
//	PUT /backup?name=<file>  store an opaque encrypted archive (create-only)
//	GET /backups             list stored archives (name, size, timestamp)
//	GET /backup/{name}       return one archive
//
// Every route — and every unknown path — requires bearer auth. Archive
// names are the T20 file names; the name in the URL is the id.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mrrobotisreal/sumrak-api/internal/auth"
	"github.com/mrrobotisreal/sumrak-api/internal/storage"
)

// Options configures a syncd handler.
type Options struct {
	Token        string
	MaxBodyBytes int64
	Log          *slog.Logger
	// Sync, when non-nil, serves the /v1 sync API (already wrapped in
	// firebaseauth.Middleware by the caller). Nil disables /v1: those paths
	// fall through to the bearer-protected mux and 404 like any unknown route.
	Sync http.Handler
}

// New returns the complete syncd http.Handler: auth wrapped around the
// routed endpoints.
func New(store *storage.Store, opts Options) http.Handler {
	s := &server{store: store, maxBody: opts.MaxBodyBytes, log: opts.Log}
	if s.log == nil {
		s.log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /backup", s.handlePut)
	mux.HandleFunc("GET /backups", s.handleList)
	mux.HandleFunc("GET /backup/{name}", s.handleGet)
	backups := auth.Middleware(opts.Token, mux)
	if opts.Sync == nil {
		return backups
	}
	// /v1/* uses Firebase ID-token auth (inside opts.Sync); everything else —
	// including unknown non-/v1 paths — keeps the bearer middleware.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			opts.Sync.ServeHTTP(w, r)
			return
		}
		backups.ServeHTTP(w, r)
	})
}

type server struct {
	store   *storage.Store
	maxBody int64
	log     *slog.Logger
}

// listedBackup is one row of GET /backups. TimestampMs comes from the name
// (UTC), matching what the app derives; it is convenience, not authority.
type listedBackup struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	TimestampMs int64  `json:"timestampMs"`
}

func (s *server) handlePut(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	body := http.MaxBytesReader(w, r.Body, s.maxBody)
	n, err := s.store.Put(name, body)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrBadName):
			s.fail(w, r, http.StatusBadRequest, "invalid backup name", err)
		case errors.Is(err, storage.ErrExists):
			s.fail(w, r, http.StatusConflict, "backup already exists", err)
		default:
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				s.fail(w, r, http.StatusRequestEntityTooLarge, "body too large", err)
				return
			}
			s.fail(w, r, http.StatusInternalServerError, "storage error", err)
		}
		return
	}

	// Retention runs after every successful store; a prune failure never
	// fails the backup itself (mirrors the T20 app-side ordering).
	pruned, pruneErr := s.store.Prune()
	if pruneErr != nil {
		s.log.Warn("prune failed", "err", pruneErr, "prunedSoFar", len(pruned))
	}
	s.log.Info("backup stored", "name", name, "bytes", n, "pruned", len(pruned))
	s.respond(w, http.StatusCreated, map[string]any{
		"stored": name,
		"bytes":  n,
		"pruned": len(pruned),
	})
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	infos, err := s.store.List()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "storage error", err)
		return
	}
	rows := make([]listedBackup, len(infos))
	for i, in := range infos {
		rows[i] = listedBackup{Name: in.Name, Size: in.Size, TimestampMs: in.Time.UnixMilli()}
	}
	s.respond(w, http.StatusOK, map[string]any{"backups": rows})
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	f, _, err := s.store.Open(name)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrBadName):
			s.fail(w, r, http.StatusBadRequest, "invalid backup name", err)
		case errors.Is(err, storage.ErrNotFound):
			s.fail(w, r, http.StatusNotFound, "backup not found", err)
		default:
			s.fail(w, r, http.StatusInternalServerError, "storage error", err)
		}
		return
	}
	defer f.Close()
	// Archives are stored verbatim from the app: an encrypted-envelope JSON.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("stream backup interrupted", "name", name, "err", err)
	}
}

func (s *server) respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Warn("write response failed", "err", err)
	}
}

// fail logs the real error and answers with a terse message — storage paths
// and internals stay out of responses.
func (s *server) fail(w http.ResponseWriter, r *http.Request, status int, msg string, err error) {
	s.log.Warn("request failed", "method", r.Method, "path", r.URL.Path, "status", status, "err", err)
	s.respond(w, status, map[string]string{"error": msg})
}
