// TLS termination helpers (phase 7, spec §15).
//
// Design: TLS terminates AT THE EDGE. For acceptor sessions the public
// pool port runs a Go tls.Listener (standard library only,
// MinVersion TLS 1.2, no custom cryptography) that decrypts bytes and
// forwards them into the existing plaintext pipeline: the TCP guard
// proxy (which keeps enforcing its byte-level protections — 8192-byte
// frame cap, per-IP rate limits, idle/logon timeouts, min-heartbeat
// guard — on the decrypted stream) dials the loopback QuickFIX/Go
// acceptor. The FIX engine itself never sees TLS; it stays plaintext
// on 127.0.0.1.
//
// Certificates: FIXLAB_TLS_CERT / FIXLAB_TLS_KEY point at PEM files.
// When unset, a self-signed certificate is generated at startup —
// documented as DEV-ONLY and logged loudly. A tls:true session with no
// usable certificate is refused with a 400 naming the problem.
package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"time"
)

// TLSSetup is the result of resolving the serving certificate at
// startup.
type TLSSetup struct {
	// Config terminates TLS on acceptor session ports. Nil when no
	// usable certificate exists (env paths set but unloadable).
	Config *tls.Config
	// Fingerprint is the SHA-256 hex of the leaf certificate DER,
	// shown to developers so they can pin/verify it out of band.
	Fingerprint string
	// SelfSigned reports that the certificate was generated at startup
	// (dev-only). Production must mount a real cert via
	// FIXLAB_TLS_CERT / FIXLAB_TLS_KEY.
	SelfSigned bool
	// Err names the problem when env-provided paths failed to load.
	// Config is nil in that case; the server still starts, but tls:true
	// sessions are refused with a 400 naming this error.
	Err error
}

// SetupServerTLS resolves the serving certificate: env-provided files
// win; otherwise a self-signed certificate is generated (dev-only).
// It never fails outright — a broken env configuration is recorded in
// TLSSetup.Err so session creation can refuse tls:true with a 400
// naming the problem instead of bricking the whole server.
func SetupServerTLS(certFile, keyFile, publicHost string, log *slog.Logger) TLSSetup {
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return TLSSetup{Err: fmt.Errorf("FIXLAB_TLS_CERT and FIXLAB_TLS_KEY must both be set")}
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return TLSSetup{Err: fmt.Errorf("load TLS key pair: %w", err)}
		}
		leaf, err := leafCert(&cert)
		if err != nil {
			return TLSSetup{Err: fmt.Errorf("parse TLS certificate: %w", err)}
		}
		if log != nil {
			log.Info("tls: serving certificate loaded from files",
				"cert", certFile, "fingerprint", Fingerprint(leaf))
		}
		return TLSSetup{
			Config:      ServerTLSConfig(cert),
			Fingerprint: Fingerprint(leaf),
		}
	}
	cert, leaf, err := GenerateSelfSigned(publicHost)
	if err != nil {
		return TLSSetup{Err: fmt.Errorf("generate self-signed certificate: %w", err)}
	}
	if log != nil {
		log.Warn("tls: FIXLAB_TLS_CERT/KEY not set — using a GENERATED SELF-SIGNED certificate (DEV ONLY). "+
			"Mount a real certificate via FIXLAB_TLS_CERT and FIXLAB_TLS_KEY for anything beyond local testing.",
			"fingerprint", Fingerprint(leaf))
	}
	return TLSSetup{
		Config:      ServerTLSConfig(cert),
		Fingerprint: Fingerprint(leaf),
		SelfSigned:  true,
	}
}

// ServerTLSConfig builds the edge TLS config: standard library only,
// TLS 1.2 minimum, no custom cryptography (spec §15).
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// Fingerprint returns the SHA-256 hex of the leaf certificate DER.
func Fingerprint(leaf *x509.Certificate) string {
	sum := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(sum[:])
}

func leafCert(cert *tls.Certificate) (*x509.Certificate, error) {
	if cert.Leaf != nil {
		return cert.Leaf, nil
	}
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("no certificate in chain")
	}
	return x509.ParseCertificate(cert.Certificate[0])
}

// GenerateSelfSigned creates a dev-only self-signed serving
// certificate (ECDSA P-256). SANs cover localhost/127.0.0.1 plus the
// public host so local clients can verify by IP or name when they
// choose to trust it.
func GenerateSelfSigned(publicHost string) (tls.Certificate, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "FixLab", Organization: []string{"FixLab (dev self-signed)"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if publicHost != "" && publicHost != "localhost" {
		if ip := net.ParseIP(publicHost); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, publicHost)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	cert.Leaf = leaf
	return cert, leaf, nil
}

// CertToPEM renders a tls.Certificate as PEM blocks (used by the
// fixacceptor test counterparty to materialize --tls key files).
func CertToPEM(cert tls.Certificate) (certPEM, keyPEM []byte, err error) {
	if len(cert.Certificate) == 0 {
		return nil, nil, fmt.Errorf("no certificate in chain")
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	switch k := cert.PrivateKey.(type) {
	case *ecdsa.PrivateKey:
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, nil, err
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	default:
		return nil, nil, fmt.Errorf("unsupported private key type %T", cert.PrivateKey)
	}
	return certPEM, keyPEM, nil
}
