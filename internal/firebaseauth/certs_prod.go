//go:build !dev

package firebaseauth

// CertsURLFromEnv returns "" (production cert endpoint) in non-dev builds;
// the SYNCD_DEV_CERTS_URL override exists only under `-tags dev`.
func CertsURLFromEnv() string { return "" }
