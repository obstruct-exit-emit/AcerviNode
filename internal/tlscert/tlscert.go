// Package tlscert generates and manages the self-signed certificate
// AcerviNode's own HTTPS listener uses — see cmd/acervinode/main.go and
// docs/providers.md's TLS section. Named tlscert, not tls, so callers that
// also need stdlib crypto/tls (every actual caller does) never need an
// import alias.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// validity is deliberately long — this is a personal-LAN self-signed cert
// with no rotation/renewal logic, matching this project's stance elsewhere
// that simplicity beats a feature nobody's asked for (see the database's own
// "acceptable to lose" precedent). Ten years outlives the hardware.
const validity = 10 * 365 * 24 * time.Hour

// EnsureCertificate returns a cert/key file pair AcerviNode's HTTPS listener
// can load, generating a self-signed one under <dataDir>/tls/ if neither an
// override nor an existing generated pair is already there.
//
// certFileOverride/keyFileOverride (both-or-neither, enforced by
// config.Config.Validate before this is ever called) let an operator bring
// their own real certificate — e.g. from Tailscale's own cert tooling —
// instead of the auto-generated self-signed one; when set, they're returned
// as-is with no generation or SAN logic at all.
//
// Otherwise, an existing generated pair is reused untouched — regenerating
// on every startup would invalidate trust every device that already clicked
// through the browser warning had to establish, for no benefit. See
// RegenerateCertificate for the one legitimate reason to force a fresh one
// (the SANs no longer match how the instance is actually reached).
func EnsureCertificate(dataDir, certFileOverride, keyFileOverride string, hosts []string) (certFile, keyFile string, err error) {
	if certFileOverride != "" && keyFileOverride != "" {
		if _, err := os.Stat(certFileOverride); err != nil {
			return "", "", fmt.Errorf("tls_cert_file %q: %w", certFileOverride, err)
		}
		if _, err := os.Stat(keyFileOverride); err != nil {
			return "", "", fmt.Errorf("tls_key_file %q: %w", keyFileOverride, err)
		}
		return certFileOverride, keyFileOverride, nil
	}

	certFile, keyFile = defaultPaths(dataDir)
	if _, certErr := os.Stat(certFile); certErr == nil {
		if _, keyErr := os.Stat(keyFile); keyErr == nil {
			return certFile, keyFile, nil
		}
	}

	if err := generate(certFile, keyFile, hosts); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

// RegenerateCertificate forces a fresh self-signed cert/key pair, replacing
// whatever's currently at the default generated location — the fix for a
// cert whose baked-in SANs no longer match how the instance is reached (a
// VM's LAN IP changing after DHCP renewal, say). Never touches a BYO
// cert_file/key_file override; regenerating something the operator supplied
// themselves isn't this function's place.
func RegenerateCertificate(dataDir string, hosts []string) (certFile, keyFile string, err error) {
	certFile, keyFile = defaultPaths(dataDir)
	if err := generate(certFile, keyFile, hosts); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

func defaultPaths(dataDir string) (certFile, keyFile string) {
	dir := filepath.Join(dataDir, "tls")
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

func generate(certFile, keyFile string, hosts []string) error {
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return fmt.Errorf("create tls dir: %w", err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "AcerviNode"},
		NotBefore:    now.Add(-time.Hour), // clock skew tolerance
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         false,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}

	return nil
}

// CollectHosts gathers every hostname/IP AcerviNode can detect about itself
// right now, for use as EnsureCertificate/RegenerateCertificate's hosts
// argument: loopback (both families), "localhost", the machine's own
// hostname, and every IP bound to a local interface — deduped. Best-effort:
// an interface enumeration failure just means a shorter (still usable) SAN
// list, not a fatal error, since the loopback/localhost/hostname entries
// alone are already enough to reach the instance from the same machine.
func CollectHosts() []string {
	seen := map[string]bool{}
	var hosts []string
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}

	add("127.0.0.1")
	add("::1")
	add("localhost")
	if name, err := os.Hostname(); err == nil {
		add(name)
	}

	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			add(ip.String())
		}
	}

	return hosts
}
