package control

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
)

type clusterTestCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	path string
}

func newClusterTestCA(t *testing.T) clusterTestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated test cluster"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return clusterTestCA{cert: cert, key: key, path: path}
}
func (ca clusterTestCA) node(t *testing.T, id string) config.Cluster {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse("spiffe://tunnel-mvp/node/" + id)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{uri}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	raw, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	return config.Cluster{Enabled: true, NodeID: id, Listen: "127.0.0.1:0", AdvertiseURL: "https://127.0.0.1:0", CAFile: ca.path, CertFile: certPath, KeyFile: keyPath}
}
func TestClusterTLSIdentityAndTrust(t *testing.T) {
	ca := newClusterTestCA(t)
	c := ca.node(t, "node-a")
	good, err := loadClusterTLS(c)
	if err != nil {
		t.Fatal(err)
	}
	if good.config.MinVersion != 0x0304 || good.caHash == "" || good.fingerprint == "" {
		t.Fatal("incomplete TLS identity")
	}
	for _, test := range []struct {
		name   string
		mutate func(*config.Cluster)
	}{
		{"wrong node", func(v *config.Cluster) { v.NodeID = "node-b" }},
		{"wrong DNS", func(v *config.Cluster) { v.AdvertiseURL = "https://other.example:8443" }},
		{"other CA", func(v *config.Cluster) { v.CAFile = newClusterTestCA(t).path }},
		{"leaf as CA", func(v *config.Cluster) { v.CAFile = v.CertFile }},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := c
			test.mutate(&copy)
			if _, err := loadClusterTLS(copy); err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}
