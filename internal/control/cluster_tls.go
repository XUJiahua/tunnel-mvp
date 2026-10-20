package control

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type clusterTLS struct {
	config              *tls.Config
	fingerprint, caHash string
}

func clusterCertificateID(cert *x509.Certificate) (string, error) {
	if cert.IsCA || len(cert.URIs) != 1 {
		return "", fmt.Errorf("cluster node certificate requires one node URI")
	}
	u := cert.URIs[0]
	if u.Scheme != "spiffe" || u.Host != "tunnel-mvp" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/node/") {
		return "", fmt.Errorf("invalid cluster node certificate URI")
	}
	id := strings.TrimPrefix(u.Path, "/node/")
	if !state.ValidClusterNodeID(id) {
		return "", fmt.Errorf("invalid cluster node identity")
	}
	return id, nil
}
func certificateFingerprint(cert *x509.Certificate) string {
	hash := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(hash[:])
}
func loadClusterTLS(c config.Cluster) (*clusterTLS, error) {
	raw, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read cluster CA: %w", err)
	}
	roots := x509.NewCertPool()
	hashes := []string{}
	for len(strings.TrimSpace(string(raw))) > 0 {
		block, rest := pem.Decode(raw)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("cluster CA must contain CA certificates only")
		}
		raw = rest
		cert, e := x509.ParseCertificate(block.Bytes)
		if e != nil || !cert.IsCA {
			return nil, fmt.Errorf("invalid cluster CA certificate")
		}
		roots.AddCert(cert)
		hashes = append(hashes, certificateFingerprint(cert))
	}
	if len(hashes) == 0 {
		return nil, fmt.Errorf("empty cluster CA")
	}
	slices.Sort(hashes)
	caHash := digest(strings.Join(hashes, "\n"))
	pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load cluster node key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	pair.Leaf = leaf
	id, err := clusterCertificateID(leaf)
	if err != nil || id != c.NodeID {
		return nil, fmt.Errorf("cluster nodeID does not match certificate URI")
	}
	advertised, err := url.Parse(c.AdvertiseURL)
	if err != nil {
		return nil, err
	}
	intermediates := x509.NewCertPool()
	for _, raw := range pair.Certificate[1:] {
		cert, e := x509.ParseCertificate(raw)
		if e != nil {
			return nil, e
		}
		intermediates.AddCert(cert)
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: advertised.Hostname(), KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			return nil, fmt.Errorf("verify cluster node certificate: %w", err)
		}
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, RootCAs: roots, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"http/1.1"}}
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("cluster peer certificate missing")
		}
		_, err := clusterCertificateID(cs.PeerCertificates[0])
		return err
	}
	return &clusterTLS{config: cfg, fingerprint: certificateFingerprint(leaf), caHash: caHash}, nil
}
