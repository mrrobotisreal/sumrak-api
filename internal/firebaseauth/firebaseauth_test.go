package firebaseauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const project = "sumrak-test"

type fixture struct {
	key   *rsa.PrivateKey
	srv   *httptest.Server
	fetch *atomic.Int32
}

func certPEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{key: key, fetch: &atomic.Int32{}}
	body, _ := json.Marshal(map[string]string{"kid1": certPEM(t, key)})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.fetch.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) mint(t *testing.T, mod func(jwt.MapClaims), key *rsa.PrivateKey) string {
	t.Helper()
	now := time.Now()
	c := jwt.MapClaims{
		"iss": "https://securetoken.google.com/" + project, "aud": project, "sub": "uid-1",
		"email": "a@example.com", "iat": now.Add(-time.Minute).Unix(), "auth_time": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	if mod != nil {
		mod(c)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = "kid1"
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerify(t *testing.T) {
	f := newFixture(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := New(project, f.srv.URL)
	future := time.Now().Add(time.Hour).Unix()
	cases := []struct {
		name string
		mod  func(jwt.MapClaims)
		key  *rsa.PrivateKey
		ok   bool
	}{
		{"valid", nil, f.key, true},
		{"wrong aud", func(c jwt.MapClaims) { c["aud"] = "other" }, f.key, false},
		{"wrong iss", func(c jwt.MapClaims) { c["iss"] = "https://securetoken.google.com/other" }, f.key, false},
		{"expired", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, f.key, false},
		{"future iat", func(c jwt.MapClaims) { c["iat"] = future }, f.key, false},
		{"future auth_time", func(c jwt.MapClaims) { c["auth_time"] = future }, f.key, false},
		{"missing sub", func(c jwt.MapClaims) { delete(c, "sub") }, f.key, false},
		{"long sub", func(c jwt.MapClaims) { c["sub"] = string(make([]byte, 129)) }, f.key, false},
		{"bad signature", nil, other, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl, err := v.Verify(context.Background(), f.mint(t, tc.mod, tc.key))
			if tc.ok && (err != nil || cl.UID != "uid-1" || cl.Email != "a@example.com") {
				t.Fatalf("want ok, got %+v %v", cl, err)
			}
			if !tc.ok && err == nil {
				t.Fatal("want rejection")
			}
		})
	}
}

func TestVerify_rejectsHS256(t *testing.T) {
	f := newFixture(t)
	v := New(project, f.srv.URL)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "https://securetoken.google.com/" + project,
		"aud": project, "sub": "x", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "auth_time": time.Now().Unix()})
	tok.Header["kid"] = "kid1"
	s, _ := tok.SignedString([]byte("secret"))
	if _, err := v.Verify(context.Background(), s); err == nil {
		t.Fatal("HS256 must be rejected")
	}
}

func TestCertsCachedByMaxAge(t *testing.T) {
	f := newFixture(t)
	v := New(project, f.srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := v.Verify(context.Background(), f.mint(t, nil, f.key)); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.fetch.Load(); n != 1 {
		t.Fatalf("cert fetches = %d, want 1", n)
	}
}

func TestMiddleware(t *testing.T) {
	f := newFixture(t)
	v := New(project, f.srv.URL)
	var got Claims
	h := Middleware(v, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
	}))
	for _, hdr := range []string{"", "Bearer", "Bearer garbage", "Basic abc"} {
		req := httptest.NewRequest("GET", "/", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("%q -> %d", hdr, rec.Code)
		}
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+f.mint(t, nil, f.key))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || got.UID != "uid-1" || got.Email != "a@example.com" {
		t.Fatalf("code %d claims %+v", rec.Code, got)
	}
}
