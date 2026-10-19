//go:build dev

package firebaseauth

import "os"

// CertsURLFromEnv honours SYNCD_DEV_CERTS_URL. Compiled ONLY with `-tags dev`
// so a production binary can never be pointed at attacker-chosen certs.
func CertsURLFromEnv() string { return os.Getenv("SYNCD_DEV_CERTS_URL") }
