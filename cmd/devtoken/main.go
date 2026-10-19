//go:build dev

// devtoken is a DEV-ONLY helper (build tag `dev`): it generates a throwaway
// RSA key, serves its x509 cert in Google's securetoken format on
// 127.0.0.1:9099, and prints a Firebase-shaped ID token for the given
// project/uid. Run syncd (also built with -tags dev) with
// SYNCD_DEV_CERTS_URL=http://127.0.0.1:9099/certs to walk the API with curl.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	project := flag.String("project", "sumrak-dev", "firebase project id")
	uid := flag.String("uid", "dev-user-1", "uid (sub)")
	email := flag.String("email", "dev@example.com", "email claim")
	addr := flag.String("addr", "127.0.0.1:9099", "cert server address")
	flag.Parse()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "devtoken"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	certs, _ := json.Marshal(map[string]string{"dev-kid": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))})

	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://securetoken.google.com/" + *project, "aud": *project, "sub": *uid, "email": *email,
		"iat": now.Add(-time.Minute).Unix(), "auth_time": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "dev-kid"
	signed, err := tok.SignedString(key)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(signed)
	http.HandleFunc("/certs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = w.Write(certs)
	})
	log.Fatal(http.ListenAndServe(*addr, nil))
}
