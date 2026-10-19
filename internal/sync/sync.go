// Package sync implements syncd's /v1 per-row sync API: device registry,
// LWW-by-(hlc, device_id) push, and seq-cursor pull, all scoped to the
// Firebase uid found in the request context (never from the body).
package sync

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/sumrak-api/internal/firebaseauth"
)

const (
	MaxChanges     = 500
	MaxPKBytes     = 512
	MaxDataBytes   = 256 << 10
	DefaultLimit   = 500
	MaxPullLimit   = 1000
	maxSmallBody   = 64 << 10
	pushLockKey    = int64(0x53796e6350757368) // serialises pushes so seq commits in order
	maxFieldLength = 256
)

// Handler serves the /v1 routes. Mount via Routes behind firebaseauth.
type Handler struct {
	pool         *pgxpool.Pool
	maxPushBytes int64
	log          *slog.Logger
}

// New builds a Handler.
func New(pool *pgxpool.Pool, maxPushBytes int64, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{pool: pool, maxPushBytes: maxPushBytes, log: log}
}

// Routes returns the /v1 mux wrapped in request logging + response gzip.
// The caller wraps it in firebaseauth.Middleware.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/devices", h.handleDevices)
	mux.HandleFunc("GET /v1/me", h.handleMe)
	mux.HandleFunc("POST /v1/sync/push", h.handlePush)
	mux.HandleFunc("GET /v1/sync/pull", h.handlePull)
	return h.logging(mux)
}

// ---- wire types ----

type deviceReq struct {
	DeviceID   string `json:"deviceId"`
	Name       string `json:"name"`
	Model      string `json:"model"`
	AppVersion string `json:"appVersion"`
}

type deviceInfo struct {
	DeviceID   string    `json:"deviceId"`
	Name       string    `json:"name"`
	Model      string    `json:"model"`
	AppVersion string    `json:"appVersion"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}

type meResp struct {
	UID         string       `json:"uid"`
	Email       string       `json:"email"`
	DeviceCount int          `json:"deviceCount"`
	RecordCount int64        `json:"recordCount"`
	MaxSeq      int64        `json:"maxSeq"`
	Devices     []deviceInfo `json:"devices,omitempty"`
}

type pushChange struct {
	Tbl  string          `json:"tbl"`
	PK   string          `json:"pk"`
	Op   string          `json:"op"`
	Data json.RawMessage `json:"data,omitempty"`
	HLC  int64           `json:"hlc"`
}

type pushReq struct {
	DeviceID      string       `json:"deviceId"`
	SchemaVersion int          `json:"schemaVersion"`
	Changes       []pushChange `json:"changes"`
}

type pushResp struct {
	Applied int   `json:"applied"`
	Skipped int   `json:"skipped"`
	MaxSeq  int64 `json:"maxSeq"`
}

type pullChange struct {
	Tbl      string          `json:"tbl"`
	PK       string          `json:"pk"`
	Op       string          `json:"op"`
	Data     json.RawMessage `json:"data,omitempty"`
	HLC      int64           `json:"hlc"`
	DeviceID string          `json:"deviceId"`
	Seq      int64           `json:"seq"`
}

type pullResp struct {
	Changes   []pullChange `json:"changes"`
	NextSince int64        `json:"nextSince"`
	HasMore   bool         `json:"hasMore"`
}

// ---- handlers ----

func (h *Handler) handleDevices(w http.ResponseWriter, r *http.Request) {
	cl, _ := firebaseauth.FromContext(r.Context())
	var req deviceReq
	if !h.decode(w, r, maxSmallBody, &req) {
		return
	}
	if req.DeviceID == "" || len(req.DeviceID) > maxFieldLength ||
		len(req.Name) > maxFieldLength || len(req.Model) > maxFieldLength || len(req.AppVersion) > maxFieldLength {
		writeErr(w, http.StatusBadRequest, "invalid device")
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
		if err := upsertUser(ctx, tx, cl); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO devices (uid, device_id, name, model, app_version)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (uid, device_id) DO UPDATE SET name=EXCLUDED.name, model=EXCLUDED.model,
			  app_version=EXCLUDED.app_version, last_seen_at=now()`,
			cl.UID, req.DeviceID, req.Name, req.Model, req.AppVersion)
		return err
	})
	if err != nil {
		h.serverErr(w, r, err)
		return
	}
	resp, err := h.me(ctx, cl, false)
	if err != nil {
		h.serverErr(w, r, err)
		return
	}
	h.stat(r).devices = resp.DeviceCount
	h.respond(w, r, http.StatusOK, resp)
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	cl, _ := firebaseauth.FromContext(r.Context())
	resp, err := h.me(r.Context(), cl, true)
	if err != nil {
		h.serverErr(w, r, err)
		return
	}
	h.respond(w, r, http.StatusOK, resp)
}

func (h *Handler) me(ctx context.Context, cl firebaseauth.Claims, withDevices bool) (meResp, error) {
	out := meResp{UID: cl.UID, Email: cl.Email}
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(seq),0) FROM records WHERE uid=$1`, cl.UID).
		Scan(&out.RecordCount, &out.MaxSeq); err != nil {
		return out, err
	}
	rows, err := h.pool.Query(ctx,
		`SELECT device_id, name, model, app_version, last_seen_at FROM devices WHERE uid=$1 ORDER BY created_at, device_id`, cl.UID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	devs := []deviceInfo{}
	for rows.Next() {
		var d deviceInfo
		if err := rows.Scan(&d.DeviceID, &d.Name, &d.Model, &d.AppVersion, &d.LastSeenAt); err != nil {
			return out, err
		}
		devs = append(devs, d)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.DeviceCount = len(devs)
	if withDevices {
		out.Devices = devs
	}
	return out, nil
}

func (h *Handler) handlePush(w http.ResponseWriter, r *http.Request) {
	cl, _ := firebaseauth.FromContext(r.Context())
	var req pushReq
	if !h.decode(w, r, h.maxPushBytes, &req) {
		return
	}
	if req.DeviceID == "" || len(req.DeviceID) > maxFieldLength {
		writeErr(w, http.StatusBadRequest, "invalid deviceId")
		return
	}
	if len(req.Changes) > MaxChanges {
		writeErr(w, http.StatusBadRequest, "too many changes")
		return
	}
	for _, c := range req.Changes {
		if !Allowed(c.Tbl) {
			writeErr(w, http.StatusBadRequest, "unknown table")
			return
		}
		if c.PK == "" || len(c.PK) > MaxPKBytes {
			writeErr(w, http.StatusBadRequest, "invalid pk")
			return
		}
		switch c.Op {
		case "upsert":
			if len(c.Data) == 0 || string(c.Data) == "null" || len(c.Data) > MaxDataBytes {
				writeErr(w, http.StatusBadRequest, "invalid data")
				return
			}
		case "delete":
		default:
			writeErr(w, http.StatusBadRequest, "invalid op")
			return
		}
	}

	ctx := r.Context()
	var resp pushResp
	err := pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
		// Serialise pushes (single-user app, small tx) so seq values commit in
		// order and a concurrent pull cursor can never skip an uncommitted row.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, pushLockKey); err != nil {
			return err
		}
		if err := upsertUser(ctx, tx, cl); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO devices (uid, device_id) VALUES ($1,$2)
			ON CONFLICT (uid, device_id) DO UPDATE SET last_seen_at=now()`, cl.UID, req.DeviceID); err != nil {
			return err
		}
		const q = `INSERT INTO records AS r (uid, tbl, pk, data, deleted, hlc, device_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (uid, tbl, pk) DO UPDATE SET data=EXCLUDED.data, deleted=EXCLUDED.deleted,
			  hlc=EXCLUDED.hlc, device_id=EXCLUDED.device_id, seq=nextval('records_seq'), updated_at=now()
			WHERE (EXCLUDED.hlc, EXCLUDED.device_id) > (r.hlc, r.device_id)`
		batch := &pgx.Batch{}
		for _, c := range req.Changes {
			var data any
			deleted := c.Op == "delete"
			if !deleted {
				data = string(c.Data)
			}
			batch.Queue(q, cl.UID, c.Tbl, c.PK, data, deleted, c.HLC, req.DeviceID)
		}
		br := tx.SendBatch(ctx, batch)
		for range req.Changes {
			tag, err := br.Exec()
			if err != nil {
				_ = br.Close()
				return err
			}
			if tag.RowsAffected() == 1 {
				resp.Applied++
			} else {
				resp.Skipped++
			}
		}
		if err := br.Close(); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0) FROM records WHERE uid=$1`, cl.UID).Scan(&resp.MaxSeq)
	})
	if err != nil {
		h.serverErr(w, r, err)
		return
	}
	st := h.stat(r)
	st.applied, st.skipped, st.changes = resp.Applied, resp.Skipped, len(req.Changes)
	h.respond(w, r, http.StatusOK, resp)
}

func (h *Handler) handlePull(w http.ResponseWriter, r *http.Request) {
	cl, _ := firebaseauth.FromContext(r.Context())
	q := r.URL.Query()
	since, limit := int64(0), DefaultLimit
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid since")
			return
		}
		since = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(n, MaxPullLimit)
	}
	rows, err := h.pool.Query(r.Context(),
		`SELECT tbl, pk, deleted, data, hlc, device_id, seq FROM records
		 WHERE uid=$1 AND seq>$2 ORDER BY seq LIMIT $3`, cl.UID, since, limit+1)
	if err != nil {
		h.serverErr(w, r, err)
		return
	}
	defer rows.Close()
	resp := pullResp{Changes: []pullChange{}, NextSince: since}
	for rows.Next() {
		var c pullChange
		var deleted bool
		var data []byte
		if err := rows.Scan(&c.Tbl, &c.PK, &deleted, &data, &c.HLC, &c.DeviceID, &c.Seq); err != nil {
			h.serverErr(w, r, err)
			return
		}
		if len(resp.Changes) == limit {
			resp.HasMore = true
			break
		}
		if deleted {
			c.Op = "delete"
		} else {
			c.Op, c.Data = "upsert", json.RawMessage(data)
		}
		resp.Changes = append(resp.Changes, c)
		resp.NextSince = c.Seq
	}
	if err := rows.Err(); err != nil {
		h.serverErr(w, r, err)
		return
	}
	h.stat(r).changes = len(resp.Changes)
	h.respond(w, r, http.StatusOK, resp)
}

func upsertUser(ctx context.Context, tx pgx.Tx, cl firebaseauth.Claims) error {
	_, err := tx.Exec(ctx, `INSERT INTO users (uid, email) VALUES ($1,$2)
		ON CONFLICT (uid) DO UPDATE SET email=COALESCE(NULLIF(EXCLUDED.email,''), users.email), last_seen_at=now()`,
		cl.UID, cl.Email)
	return err
}

// ---- request / response plumbing ----

// decode reads a JSON body (optionally gzip) capped at limit bytes both on
// the wire and after decompression; it writes the error response itself.
func (h *Handler) decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	var body io.Reader = http.MaxBytesReader(w, r.Body, limit)
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid gzip body")
			return false
		}
		defer zr.Close()
		body = &capReader{r: zr, left: limit}
	default:
		writeErr(w, http.StatusUnsupportedMediaType, "unsupported Content-Encoding")
		return false
	}
	if err := json.NewDecoder(body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) || errors.Is(err, errTooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid json")
		}
		return false
	}
	return true
}

var errTooLarge = errors.New("decompressed body too large")

type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *Handler) respond(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	var out io.Writer = w
	if acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		w.WriteHeader(status)
		zw := gzip.NewWriter(w)
		defer zw.Close()
		out = zw
	} else {
		w.WriteHeader(status)
	}
	if err := json.NewEncoder(out).Encode(body); err != nil {
		h.log.Warn("write response failed", "err", err)
	}
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(name, "gzip") {
			return true
		}
	}
	return false
}

func (h *Handler) serverErr(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Error("v1 request failed", "route", r.Method+" "+r.URL.Path, "err", err)
	if errors.Is(err, context.Canceled) {
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal error")
}

// ---- logging ----

type reqStats struct {
	changes, applied, skipped, devices int
}

type statKey struct{}

func (h *Handler) stat(r *http.Request) *reqStats {
	if s, ok := r.Context().Value(statKey{}).(*reqStats); ok {
		return s
	}
	return &reqStats{}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }

// logging emits one slog line per /v1 request: route, status, ms, uid hash,
// counts. Never row data, never tokens.
func (h *Handler) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		st := &reqStats{}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), statKey{}, st)))
		uid := ""
		if cl, ok := firebaseauth.FromContext(r.Context()); ok {
			sum := sha256.Sum256([]byte(cl.UID))
			uid = hex.EncodeToString(sum[:])[:8]
		}
		h.log.Info("v1",
			"route", r.Method+" "+r.URL.Path, "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "uid", uid,
			"changes", st.changes, "applied", st.applied, "skipped", st.skipped)
	})
}
