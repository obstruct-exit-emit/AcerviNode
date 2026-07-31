package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureCertificate_GeneratesFreshCertWithExpectedSANs(t *testing.T) {
	dataDir := t.TempDir()
	hosts := []string{"127.0.0.1", "::1", "localhost", "my-host", "192.168.1.50"}

	certFile, keyFile, err := EnsureCertificate(dataDir, "", "", hosts)
	if err != nil {
		t.Fatalf("EnsureCertificate() error = %v", err)
	}

	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("tls.LoadX509KeyPair() error = %v", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("x509.ParseCertificate() error = %v", err)
	}

	wantIPs := map[string]bool{"127.0.0.1": false, "::1": false, "192.168.1.50": false}
	for _, ip := range cert.IPAddresses {
		if _, ok := wantIPs[ip.String()]; ok {
			wantIPs[ip.String()] = true
		}
	}
	for ip, found := range wantIPs {
		if !found {
			t.Errorf("cert.IPAddresses = %v, want it to include %s", cert.IPAddresses, ip)
		}
	}

	wantDNS := map[string]bool{"localhost": false, "my-host": false}
	for _, name := range cert.DNSNames {
		if _, ok := wantDNS[name]; ok {
			wantDNS[name] = true
		}
	}
	for name, found := range wantDNS {
		if !found {
			t.Errorf("cert.DNSNames = %v, want it to include %s", cert.DNSNames, name)
		}
	}

	wantExpiry := time.Now().Add(validity)
	if diff := cert.NotAfter.Sub(wantExpiry); diff > time.Hour || diff < -time.Hour {
		t.Errorf("cert.NotAfter = %v, want ~%v (10 years out)", cert.NotAfter, wantExpiry)
	}

	if cert.IsCA {
		t.Error("cert.IsCA = true, want a leaf (non-CA) certificate")
	}
}

func TestEnsureCertificate_ReusesExistingCertWithoutRegenerating(t *testing.T) {
	dataDir := t.TempDir()

	certFile1, keyFile1, err := EnsureCertificate(dataDir, "", "", []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("first EnsureCertificate() error = %v", err)
	}
	certBytes1, err := os.ReadFile(certFile1)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}

	// A second call, even with a different hosts list, must not regenerate —
	// see EnsureCertificate's own doc comment on why silently invalidating
	// already-trusted certs is the wrong default.
	certFile2, keyFile2, err := EnsureCertificate(dataDir, "", "", []string{"10.0.0.99"})
	if err != nil {
		t.Fatalf("second EnsureCertificate() error = %v", err)
	}
	certBytes2, err := os.ReadFile(certFile2)
	if err != nil {
		t.Fatalf("read cert (second time): %v", err)
	}

	if certFile1 != certFile2 || keyFile1 != keyFile2 {
		t.Errorf("paths changed between calls: (%s,%s) vs (%s,%s)", certFile1, keyFile1, certFile2, keyFile2)
	}
	if string(certBytes1) != string(certBytes2) {
		t.Error("cert bytes changed between calls, want the existing cert reused untouched")
	}
}

func TestEnsureCertificate_BYOOverrideReturnsAsIsWithoutGenerating(t *testing.T) {
	dataDir := t.TempDir()
	certFile := filepath.Join(dataDir, "my-cert.pem")
	keyFile := filepath.Join(dataDir, "my-key.pem")
	if err := os.WriteFile(certFile, []byte("not a real cert, just proving passthrough"), 0o644); err != nil {
		t.Fatalf("write fake cert: %v", err)
	}
	if err := os.WriteFile(keyFile, []byte("not a real key, just proving passthrough"), 0o600); err != nil {
		t.Fatalf("write fake key: %v", err)
	}

	gotCert, gotKey, err := EnsureCertificate(dataDir, certFile, keyFile, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("EnsureCertificate() error = %v", err)
	}
	if gotCert != certFile || gotKey != keyFile {
		t.Errorf("EnsureCertificate() = (%s, %s), want the override paths returned unchanged", gotCert, gotKey)
	}

	// Nothing should have been generated under <dataDir>/tls/.
	if _, err := os.Stat(filepath.Join(dataDir, "tls")); err == nil {
		t.Error("tls/ directory was created despite a BYO override being supplied")
	}
}

func TestEnsureCertificate_MissingBYOFileErrors(t *testing.T) {
	dataDir := t.TempDir()
	_, _, err := EnsureCertificate(dataDir, filepath.Join(dataDir, "missing-cert.pem"), filepath.Join(dataDir, "missing-key.pem"), nil)
	if err == nil {
		t.Fatal("EnsureCertificate() error = nil, want an error for a nonexistent override file")
	}
}

func TestRegenerateCertificate_ReplacesExistingCert(t *testing.T) {
	dataDir := t.TempDir()

	certFile1, _, err := EnsureCertificate(dataDir, "", "", []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("EnsureCertificate() error = %v", err)
	}
	certBytes1, err := os.ReadFile(certFile1)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}

	certFile2, keyFile2, err := RegenerateCertificate(dataDir, []string{"10.0.0.99"})
	if err != nil {
		t.Fatalf("RegenerateCertificate() error = %v", err)
	}
	if certFile2 != certFile1 {
		t.Errorf("RegenerateCertificate() path = %s, want the same default path %s", certFile2, certFile1)
	}
	certBytes2, err := os.ReadFile(certFile2)
	if err != nil {
		t.Fatalf("read cert (after regenerate): %v", err)
	}
	if string(certBytes1) == string(certBytes2) {
		t.Error("cert bytes unchanged after RegenerateCertificate(), want a genuinely fresh cert")
	}

	pair, err := tls.LoadX509KeyPair(certFile2, keyFile2)
	if err != nil {
		t.Fatalf("regenerated cert/key don't load as a valid pair: %v", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("x509.ParseCertificate() error = %v", err)
	}
	found := false
	for _, ip := range cert.IPAddresses {
		if ip.String() == "10.0.0.99" {
			found = true
		}
	}
	if !found {
		t.Errorf("regenerated cert.IPAddresses = %v, want it to include the new host 10.0.0.99", cert.IPAddresses)
	}
}

func TestCollectHosts_IncludesLoopbackAndHostname(t *testing.T) {
	hosts := CollectHosts()

	want := map[string]bool{"127.0.0.1": false, "::1": false, "localhost": false}
	for _, h := range hosts {
		if _, ok := want[h]; ok {
			want[h] = true
		}
	}
	for h, found := range want {
		if !found {
			t.Errorf("CollectHosts() = %v, want it to include %s", hosts, h)
		}
	}

	hostname, err := os.Hostname()
	if err == nil {
		hasHostname := false
		for _, h := range hosts {
			if h == hostname {
				hasHostname = true
			}
		}
		if !hasHostname {
			t.Errorf("CollectHosts() = %v, want it to include this machine's hostname %q", hosts, hostname)
		}
	}
}

func TestCollectHosts_NoDuplicates(t *testing.T) {
	hosts := CollectHosts()
	seen := map[string]bool{}
	for _, h := range hosts {
		if seen[h] {
			t.Errorf("CollectHosts() = %v contains duplicate %q", hosts, h)
		}
		seen[h] = true
	}
}
