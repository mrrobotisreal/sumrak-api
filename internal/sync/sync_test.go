package sync

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	gosync "sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/sumrak-api/internal/db"
	"github.com/mrrobotisreal/sumrak-api/internal/firebaseauth"
)

// fakeVerifier maps the bearer string directly to a uid ("Bearer alice").
type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, tok string) (firebaseauth.Claims, error) {
	return firebaseauth.Claims{UID: tok, Email: tok + "@example.com"}, nil
}

var testPool *pgxpool.Pool

func setup(t *testing.T, maxPush int64) http.Handler {
	t.Helper()
	url := os.Getenv("SYNCD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SYNCD_TEST_DATABASE_URL unset; run `make test-pg`")
	}
	ctx := context.Background()
	if testPool == nil {
		p, err := db.Open(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Migrate(ctx, p); err != nil {
			t.Fatal(err)
		}
		testPool = p
	}
	if _, err := testPool.Exec(ctx, `TRUNCATE users CASCADE`); err != nil {
		t.Fatal(err)
	}
	return firebaseauth.Middleware(fakeVerifier{}, New(testPool, maxPush, nil).Routes())
}

func TestMain(m *testing.M) {
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

func call(h http.Handler, uid, method, target string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req := httptest.NewRequest(method, target, rd)
	if uid != "" {
		req.Header.Set("Authorization", "Bearer "+uid)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func up(tbl, pk string, hlc int64, data string) map[string]any {
	return map[string]any{"tbl": tbl, "pk": pk, "op": "upsert", "hlc": hlc, "data": json.RawMessage(data)}
}

func push(t *testing.T, h http.Handler, uid, dev string, changes ...map[string]any) pushResp {
	t.Helper()
	rec := call(h, uid, "POST", "/v1/sync/push", map[string]any{"deviceId": dev, "schemaVersion": 1, "changes": changes}, nil)
	if rec.Code != 200 {
		t.Fatalf("push %d %s", rec.Code, rec.Body)
	}
	var r pushResp
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return r
}

func pull(t *testing.T, h http.Handler, uid string, since int64, limit int) pullResp {
	t.Helper()
	rec := call(h, uid, "GET", fmt.Sprintf("/v1/sync/pull?since=%d&limit=%d", since, limit), nil, nil)
	if rec.Code != 200 {
		t.Fatalf("pull %d %s", rec.Code, rec.Body)
	}
	var r pullResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUnauthenticated(t *testing.T) {
	h := setup(t, 1<<20)
	if rec := call(h, "", "GET", "/v1/me", nil, nil); rec.Code != 401 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestDevicesAndMe(t *testing.T) {
	h := setup(t, 1<<20)
	rec := call(h, "alice", "POST", "/v1/devices", map[string]string{"deviceId": "d1", "name": "S24U", "model": "SM-S928U", "appVersion": "1.19.0"}, nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var me meResp
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.UID != "alice" || me.Email != "alice@example.com" || me.DeviceCount != 1 || me.RecordCount != 0 || me.MaxSeq != 0 {
		t.Fatalf("devices resp %+v", me)
	}
	call(h, "alice", "POST", "/v1/devices", map[string]string{"deviceId": "d2", "name": "S25"}, nil)
	call(h, "alice", "POST", "/v1/devices", map[string]string{"deviceId": "d1", "name": "S24U renamed", "appVersion": "1.20.0"}, nil)
	push(t, h, "alice", "d1", up("notes", "n1", 1, `{"a":1}`))
	rec = call(h, "alice", "GET", "/v1/me", nil, nil)
	me = meResp{}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.DeviceCount != 2 || len(me.Devices) != 2 || me.RecordCount != 1 || me.MaxSeq < 1 || me.Devices[0].Name != "S24U renamed" || me.Devices[0].AppVersion != "1.20.0" {
		t.Fatalf("me %+v", me)
	}
	if rec := call(h, "alice", "POST", "/v1/devices", map[string]string{"name": "x"}, nil); rec.Code != 400 {
		t.Fatalf("missing deviceId -> %d", rec.Code)
	}
	// /v1/me for a brand-new uid works (zeroes).
	rec = call(h, "bob", "GET", "/v1/me", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("bob me %d", rec.Code)
	}
}

func TestLWW(t *testing.T) {
	h := setup(t, 1<<20)
	r := push(t, h, "alice", "dA", up("notes", "n1", 10, `{"v":"first"}`))
	if r.Applied != 1 || r.Skipped != 0 {
		t.Fatalf("%+v", r)
	}
	// older hlc skipped
	r = push(t, h, "alice", "dB", up("notes", "n1", 9, `{"v":"older"}`))
	if r.Applied != 0 || r.Skipped != 1 {
		t.Fatalf("older: %+v", r)
	}
	// tie on hlc: higher device id wins; lower loses
	r = push(t, h, "alice", "dB", up("notes", "n1", 10, `{"v":"tie-B"}`))
	if r.Applied != 1 {
		t.Fatalf("tie higher device should win: %+v", r)
	}
	r = push(t, h, "alice", "dA", up("notes", "n1", 10, `{"v":"tie-A"}`))
	if r.Applied != 0 || r.Skipped != 1 {
		t.Fatalf("tie lower device should lose: %+v", r)
	}
	// identical (hlc, device) is not strictly greater -> skipped (idempotent retry)
	r = push(t, h, "alice", "dB", up("notes", "n1", 10, `{"v":"tie-B"}`))
	if r.Skipped != 1 {
		t.Fatalf("replay: %+v", r)
	}
	// newer hlc wins
	push(t, h, "alice", "dA", up("notes", "n1", 11, `{"v":"newest"}`))
	p := pull(t, h, "alice", 0, 10)
	if len(p.Changes) != 1 || string(p.Changes[0].Data) != `{"v":"newest"}` || p.Changes[0].HLC != 11 || p.Changes[0].DeviceID != "dA" || p.Changes[0].Op != "upsert" {
		t.Fatalf("pull %+v", p)
	}
}

func TestTombstones(t *testing.T) {
	h := setup(t, 1<<20)
	push(t, h, "alice", "d1", up("bookmarks", "b1", 1, `{"x":1}`))
	r := push(t, h, "alice", "d1", map[string]any{"tbl": "bookmarks", "pk": "b1", "op": "delete", "hlc": 2})
	if r.Applied != 1 {
		t.Fatalf("%+v", r)
	}
	p := pull(t, h, "alice", 0, 10)
	if len(p.Changes) != 1 || p.Changes[0].Op != "delete" || p.Changes[0].Data != nil {
		t.Fatalf("tombstone pull %+v", p)
	}
	// stale upsert cannot resurrect
	r = push(t, h, "alice", "d2", up("bookmarks", "b1", 1, `{"x":2}`))
	if r.Skipped != 1 {
		t.Fatalf("resurrect %+v", r)
	}
	// newer upsert does
	push(t, h, "alice", "d2", up("bookmarks", "b1", 3, `{"x":3}`))
	p = pull(t, h, "alice", 0, 10)
	if p.Changes[0].Op != "upsert" || string(p.Changes[0].Data) != `{"x":3}` {
		t.Fatalf("revive %+v", p)
	}
	// delete of a never-seen row creates a tombstone
	push(t, h, "alice", "d1", map[string]any{"tbl": "notes", "pk": "ghost", "op": "delete", "hlc": 5})
	if p = pull(t, h, "alice", 0, 10); len(p.Changes) != 2 {
		t.Fatalf("ghost tombstone %+v", p)
	}
}

func TestPullPagination(t *testing.T) {
	h := setup(t, 1<<20)
	var cs []map[string]any
	for i := 0; i < 7; i++ {
		cs = append(cs, up("notes", fmt.Sprintf("n%d", i), int64(i+1), `{}`))
	}
	push(t, h, "alice", "d1", cs...)
	var seen []string
	since := int64(0)
	for pages := 0; ; pages++ {
		p := pull(t, h, "alice", since, 3)
		for _, c := range p.Changes {
			if c.Seq <= since {
				t.Fatalf("seq %d <= since %d", c.Seq, since)
			}
			seen = append(seen, c.PK)
		}
		if p.NextSince < since {
			t.Fatal("nextSince went backwards")
		}
		since = p.NextSince
		if !p.HasMore {
			if pages != 2 {
				t.Fatalf("pages = %d", pages)
			}
			break
		}
		if len(p.Changes) != 3 {
			t.Fatalf("full page expected, got %d", len(p.Changes))
		}
	}
	if len(seen) != 7 {
		t.Fatalf("saw %v", seen)
	}
	// exact multiple: limit 7 -> hasMore false
	if p := pull(t, h, "alice", 0, 7); p.HasMore || len(p.Changes) != 7 {
		t.Fatalf("exact page %+v", p)
	}
	// caught up: empty page keeps cursor
	p := pull(t, h, "alice", since, 3)
	if len(p.Changes) != 0 || p.NextSince != since || p.HasMore || p.Changes == nil {
		t.Fatalf("empty %+v", p)
	}
	// updating a row moves it past the cursor
	push(t, h, "alice", "d1", up("notes", "n0", 100, `{"u":1}`))
	p = pull(t, h, "alice", since, 3)
	if len(p.Changes) != 1 || p.Changes[0].PK != "n0" {
		t.Fatalf("update %+v", p)
	}
	// limit clamp + bad params
	if rec := call(h, "alice", "GET", "/v1/sync/pull?since=abc", nil, nil); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	if rec := call(h, "alice", "GET", "/v1/sync/pull?limit=0", nil, nil); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	if rec := call(h, "alice", "GET", "/v1/sync/pull?limit=99999", nil, nil); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
}

func TestCursorMonotonicUnderConcurrentPushers(t *testing.T) {
	h := setup(t, 1<<20)
	const perPusher, batches = 20, 5
	var wg gosync.WaitGroup
	stop := make(chan struct{})
	pullerDone := make(chan []string)
	// Puller: follows the cursor continuously; must eventually see every row,
	// and every returned seq must be strictly > the previous one.
	go func() {
		seen := map[string]bool{}
		since := int64(0)
		last := int64(0)
		var order []string
		drain := func() {
			for {
				p := pull(t, h, "alice", since, 7)
				for _, c := range p.Changes {
					if c.Seq <= last {
						t.Errorf("non-monotonic seq %d after %d", c.Seq, last)
					}
					last = c.Seq
					key := c.Tbl + "/" + c.PK
					if !seen[key] {
						seen[key] = true
						order = append(order, key)
					}
				}
				since = p.NextSince
				if !p.HasMore {
					return
				}
			}
		}
		for {
			select {
			case <-stop:
				drain()
				pullerDone <- order
				return
			default:
				drain()
			}
		}
	}()
	for _, dev := range []string{"dA", "dB"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := 0; b < batches; b++ {
				var cs []map[string]any
				for i := 0; i < perPusher; i++ {
					cs = append(cs, up("notes", fmt.Sprintf("%s-%d-%d", dev, b, i), int64(b*100+i+1), `{}`))
				}
				push(t, h, "alice", dev, cs...)
			}
		}()
	}
	wg.Wait()
	close(stop)
	order := <-pullerDone
	if len(order) != 2*perPusher*batches {
		t.Fatalf("puller saw %d rows, want %d (cursor skipped rows)", len(order), 2*perPusher*batches)
	}
}

func TestPerUIDIsolation(t *testing.T) {
	h := setup(t, 1<<20)
	push(t, h, "alice", "d1", up("notes", "same", 1, `{"owner":"alice"}`))
	push(t, h, "bob", "d9", up("notes", "same", 1, `{"owner":"bob"}`), up("notes", "bobonly", 1, `{}`))
	pa := pull(t, h, "alice", 0, 100)
	if len(pa.Changes) != 1 || string(pa.Changes[0].Data) != `{"owner":"alice"}` {
		t.Fatalf("alice sees %+v", pa)
	}
	pb := pull(t, h, "bob", 0, 100)
	if len(pb.Changes) != 2 {
		t.Fatalf("bob sees %+v", pb)
	}
	var me meResp
	_ = json.Unmarshal(call(h, "alice", "GET", "/v1/me", nil, nil).Body.Bytes(), &me)
	if me.RecordCount != 1 {
		t.Fatalf("alice me %+v", me)
	}
}

func TestOwnRowsIncludedInPull(t *testing.T) {
	h := setup(t, 1<<20)
	push(t, h, "alice", "d1", up("notes", "n", 1, `{}`))
	if p := pull(t, h, "alice", 0, 10); len(p.Changes) != 1 || p.Changes[0].DeviceID != "d1" {
		t.Fatalf("%+v", p)
	}
}

func TestValidation(t *testing.T) {
	h := setup(t, 1<<20)
	bad := map[string]map[string]any{
		"unknown table": up("sync_outbox", "x", 1, `{}`),
		"empty pk":      up("notes", "", 1, `{}`),
		"long pk":       up("notes", string(bytes.Repeat([]byte("k"), 513)), 1, `{}`),
		"bad op":        {"tbl": "notes", "pk": "x", "op": "merge", "hlc": 1},
		"missing data":  {"tbl": "notes", "pk": "x", "op": "upsert", "hlc": 1},
		"big data":      up("notes", "x", 1, `"`+string(bytes.Repeat([]byte("a"), MaxDataBytes))+`"`),
	}
	for name, c := range bad {
		rec := call(h, "alice", "POST", "/v1/sync/push", map[string]any{"deviceId": "d", "changes": []any{c}}, nil)
		if rec.Code != 400 {
			t.Errorf("%s -> %d %s", name, rec.Code, rec.Body)
		}
	}
	// one bad change rejects the whole push: nothing applied
	rec := call(h, "alice", "POST", "/v1/sync/push", map[string]any{"deviceId": "d", "changes": []any{up("notes", "ok", 1, `{}`), up("nope", "x", 1, `{}`)}}, nil)
	if rec.Code != 400 || len(pull(t, h, "alice", 0, 10).Changes) != 0 {
		t.Fatalf("partial apply: %d", rec.Code)
	}
	// 501 changes
	var many []any
	for i := 0; i <= MaxChanges; i++ {
		many = append(many, up("notes", fmt.Sprint(i), 1, `{}`))
	}
	if rec := call(h, "alice", "POST", "/v1/sync/push", map[string]any{"deviceId": "d", "changes": many}, nil); rec.Code != 400 {
		t.Errorf("501 changes -> %d", rec.Code)
	}
	if rec := call(h, "alice", "POST", "/v1/sync/push", []byte(`{not json`), nil); rec.Code != 400 {
		t.Errorf("bad json -> %d", rec.Code)
	}
	if rec := call(h, "alice", "POST", "/v1/sync/push", map[string]any{"changes": []any{}}, nil); rec.Code != 400 {
		t.Errorf("missing deviceId -> %d", rec.Code)
	}
	// empty push is fine
	if r := push(t, h, "alice", "d"); r.Applied != 0 {
		t.Errorf("%+v", r)
	}
}

func gz(t *testing.T, b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

func TestSizeLimit413(t *testing.T) {
	h := setup(t, 2048)
	big, _ := json.Marshal(map[string]any{"deviceId": "d", "changes": []any{up("notes", "x", 1, `"`+string(bytes.Repeat([]byte("a"), 5000))+`"`)}})
	if rec := call(h, "alice", "POST", "/v1/sync/push", big, nil); rec.Code != 413 {
		t.Fatalf("plain -> %d", rec.Code)
	}
	// gzip bomb: tiny on the wire, huge decompressed
	if rec := call(h, "alice", "POST", "/v1/sync/push", gz(t, big), map[string]string{"Content-Encoding": "gzip"}); rec.Code != 413 {
		t.Fatalf("gzip bomb -> %d", rec.Code)
	}
}

func TestGzipInAndOut(t *testing.T) {
	h := setup(t, 1<<20)
	body, _ := json.Marshal(map[string]any{"deviceId": "d1", "schemaVersion": 1, "changes": []any{up("notes", "n1", 1, `{"k":"v"}`)}})
	rec := call(h, "alice", "POST", "/v1/sync/push", gz(t, body), map[string]string{"Content-Encoding": "gzip"})
	if rec.Code != 200 {
		t.Fatalf("gzip in -> %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("response gzipped without Accept-Encoding")
	}
	rec = call(h, "alice", "GET", "/v1/sync/pull", nil, map[string]string{"Accept-Encoding": "gzip"})
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("headers %v", rec.Header())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	var p pullResp
	if err := json.Unmarshal(raw, &p); err != nil || len(p.Changes) != 1 || string(p.Changes[0].Data) != `{"k":"v"}` {
		t.Fatalf("gz out %s %v", raw, err)
	}
	if rec := call(h, "alice", "POST", "/v1/sync/push", body, map[string]string{"Content-Encoding": "br"}); rec.Code != 415 {
		t.Fatalf("br -> %d", rec.Code)
	}
	if rec := call(h, "alice", "POST", "/v1/sync/push", []byte("notgzip"), map[string]string{"Content-Encoding": "gzip"}); rec.Code != 400 {
		t.Fatalf("bad gzip -> %d", rec.Code)
	}
}
