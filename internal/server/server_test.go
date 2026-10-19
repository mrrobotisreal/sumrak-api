package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/sumrak-api/internal/storage"
)

const token = "test-token-0123456789abcdef"

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	store, err := storage.New(t.TempDir(), 30)
	if err != nil {
		t.Fatal(err)
	}
	return New(store, Options{Token: token, MaxBodyBytes: 1 << 20})
}

func do(h http.Handler, method, target, body string, authed bool) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if authed {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEveryEndpointRequiresAuth(t *testing.T) {
	h := newHandler(t)
	for _, c := range []struct{ method, target string }{
		{http.MethodPut, "/backup?name=sumrak-backup-20260823-120000Z.json"},
		{http.MethodGet, "/backups"},
		{http.MethodGet, "/backup/sumrak-backup-20260823-120000Z.json"},
		{http.MethodGet, "/anything-else"}, // unknown paths leak nothing either
	} {
		if rec := do(h, c.method, c.target, "x", false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated: expected 401, got %d", c.method, c.target, rec.Code)
		}
	}
}

func TestPutListGetRoundTrip(t *testing.T) {
	h := newHandler(t)
	name := "sumrak-backup-20260823-120000Z.json"
	payload := `{"v":1,"cipher":{"saltB64":"abc"}}`

	rec := do(h, http.MethodPut, "/backup?name="+name, payload, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d (%s)", rec.Code, rec.Body)
	}
	var putResp struct {
		Stored string `json:"stored"`
		Bytes  int64  `json:"bytes"`
		Pruned int    `json:"pruned"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &putResp); err != nil {
		t.Fatal(err)
	}
	if putResp.Stored != name || putResp.Bytes != int64(len(payload)) {
		t.Fatalf("unexpected PUT response: %+v", putResp)
	}

	rec = do(h, http.MethodGet, "/backups", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /backups: expected 200, got %d", rec.Code)
	}
	var listResp struct {
		Backups []struct {
			Name        string `json:"name"`
			Size        int64  `json:"size"`
			TimestampMs int64  `json:"timestampMs"`
		} `json:"backups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	wantTs := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC).UnixMilli()
	if len(listResp.Backups) != 1 || listResp.Backups[0].Name != name ||
		listResp.Backups[0].Size != int64(len(payload)) || listResp.Backups[0].TimestampMs != wantTs {
		t.Fatalf("unexpected listing: %+v", listResp)
	}

	rec = do(h, http.MethodGet, "/backup/"+name, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /backup/{name}: expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != payload {
		t.Fatalf("archive not returned verbatim: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("unexpected content type %q", ct)
	}
}

func TestPutRejectsBadAndDuplicateNames(t *testing.T) {
	h := newHandler(t)
	if rec := do(h, http.MethodPut, "/backup?name=evil.json", "x", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name: expected 400, got %d", rec.Code)
	}
	if rec := do(h, http.MethodPut, "/backup", "x", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing name: expected 400, got %d", rec.Code)
	}
	name := "sumrak-backup-20260823-120000Z.json"
	if rec := do(h, http.MethodPut, "/backup?name="+name, "x", true); rec.Code != http.StatusCreated {
		t.Fatalf("first PUT: expected 201, got %d", rec.Code)
	}
	if rec := do(h, http.MethodPut, "/backup?name="+name, "y", true); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate PUT: expected 409, got %d", rec.Code)
	}
}

func TestGetMissingAndBadNames(t *testing.T) {
	h := newHandler(t)
	if rec := do(h, http.MethodGet, "/backup/sumrak-backup-20260823-120000Z.json", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: expected 404, got %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/backup/notes.txt", "", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name: expected 400, got %d", rec.Code)
	}
}

func TestPutBodyTooLarge(t *testing.T) {
	store, err := storage.New(t.TempDir(), 30)
	if err != nil {
		t.Fatal(err)
	}
	h := New(store, Options{Token: token, MaxBodyBytes: 10})
	rec := do(h, http.MethodPut, "/backup?name=sumrak-backup-20260823-120000Z.json",
		"this body is longer than ten bytes", true)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestPutTriggersRetentionPrune(t *testing.T) {
	store, err := storage.New(t.TempDir(), 30)
	if err != nil {
		t.Fatal(err)
	}
	h := New(store, Options{Token: token, MaxBodyBytes: 1 << 20})
	// 39 stored days, then the 40th PUT pushes the window: per the T20 case,
	// 40 consecutive days → 9 July files collapse to the July keeper.
	for i := 0; i < 40; i++ {
		d := time.Date(2026, 7, 20+i, 3, 0, 0, 0, time.UTC)
		name := fmt.Sprintf("sumrak-backup-%sZ.json", d.Format("20060102-150405"))
		rec := do(h, http.MethodPut, "/backup?name="+name, "x", true)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %d: expected 201, got %d", i, rec.Code)
		}
	}
	infos, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 31 { // 30 newest dates + July monthly keeper
		t.Fatalf("expected 31 survivors after rolling prunes, got %d", len(infos))
	}
}

func TestV1DisabledWithoutSync(t *testing.T) {
	h := newHandler(t) // Options.Sync == nil
	for _, p := range []string{"/v1/me", "/v1/sync/pull", "/v1/devices"} {
		if rec := do(h, "GET", p, "", true); rec.Code != http.StatusNotFound {
			t.Fatalf("%s authed = %d, want 404", p, rec.Code)
		}
		if rec := do(h, "GET", p, "", false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthed = %d, want 401 (unchanged)", p, rec.Code)
		}
	}
}

func TestV1RoutedToSyncHandlerBackupsKeepBearer(t *testing.T) {
	store, err := storage.New(t.TempDir(), 30)
	if err != nil {
		t.Fatal(err)
	}
	sync := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := New(store, Options{Token: token, MaxBodyBytes: 1 << 20, Sync: sync})
	if rec := do(h, "GET", "/v1/me", "", false); rec.Code != http.StatusTeapot {
		t.Fatalf("/v1 should reach sync handler, got %d", rec.Code)
	}
	if rec := do(h, "GET", "/backups", "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/backups unauthed = %d", rec.Code)
	}
	if rec := do(h, "GET", "/nope", "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown path unauthed = %d", rec.Code)
	}
	if rec := do(h, "GET", "/backups", "", true); rec.Code != http.StatusOK {
		t.Fatalf("/backups authed = %d", rec.Code)
	}
}
