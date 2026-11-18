// Package webtls serves :9080 over TLS and plain HTTP on the same port.
package webtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A local CA, trusted once on each client, signs a server certificate that can be renewed
// at will (expiry, new IP) without the clients noticing.
const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 365 * 24 * time.Hour // Apple refuses longer than 825 days
	renewSoon    = 30 * 24 * time.Hour
)

var (
	stableIPv4 = interfaceIPv4
	hostname   = os.Hostname
)

// Info describes the certificate in use, for the Control Panel.
type Info struct {
	Source      string    `json:"source"`             // "self-signed" | "file"
	Fingerprint string    `json:"fingerprint_sha256"` // of the local CA when self-signed: what clients trust
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"` // of the server certificate
	CANotAfter  time.Time `json:"ca_not_after,omitzero"`
	SAN         []string  `json:"san"`
	Path        string    `json:"path"`
}

// Virtual bridges come and go (Docker, libvirt): their addresses must not force a new certificate.
var virtualIface = []string{"docker", "br-", "veth", "virbr", "cni", "flannel"}

// interfaceIPv4: IPv4 of up interfaces, no loopback, no point-to-point (wg0), no link-local,
// no virtual bridges. IPv6 is left out: privacy addresses and provider prefixes rotate.
func interfaceIPv4() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, it := range ifs {
		if it.Flags&net.FlagUp == 0 || it.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 ||
			slices.ContainsFunc(virtualIface, func(p string) bool { return strings.HasPrefix(it.Name, p) }) {
			continue
		}
		addrs, _ := it.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if v4 := n.IP.To4(); v4 != nil && !v4.IsLinkLocalUnicast() {
					out = append(out, v4.String())
				}
			}
		}
	}
	return out
}

func trimmed(in []string) []string {
	var out []string
	for _, e := range in {
		if e = strings.TrimSpace(e); e != "" && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// Required: names the server certificate must cover, or it is renewed. In a container its
// own IP changes on every re-creation and is not what clients use, so only the extra names count.
func Required(extra []string) []string {
	out := trimmed(extra)
	if len(out) > 0 && inContainer() {
		return out
	}
	for _, ip := range stableIPv4() {
		if !slices.Contains(out, ip) {
			out = append(out, ip)
		}
	}
	return out
}

var inContainer = func() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// Hosts is the SAN of the server certificate: names, loopback, the host's addresses, extra.
func Hosts(extra []string) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if h, err := hostname(); err == nil && h != "" {
		add(h)
		if !strings.Contains(h, ".") {
			add(h + ".local")
		}
	}
	add("localhost")
	add("127.0.0.1")
	add("::1")
	for _, ip := range stableIPv4() {
		add(ip)
	}
	for _, e := range extra {
		add(e)
	}
	return out
}

type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func encode(der []byte, key *ecdsa.PrivateKey) (certPEM, keyPEM []byte, err error) {
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

func generateCA(name string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Tiramisu local CA (" + name + ")", Organization: []string{"Tiramisu"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return encode(der, key)
}

func generateLeaf(ca authority, hosts []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0], Organization: []string{"Tiramisu"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	return encode(der, key)
}

func loadCA(certFile, keyFile string) (authority, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return authority{}, err
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || pair.Leaf == nil || !pair.Leaf.IsCA {
		return authority{}, errors.New("not a CA")
	}
	return authority{pair.Leaf, key}, nil
}

// needsRenewal: expiring within 30 days, not signed by ca, or missing a required name.
func needsRenewal(leaf, ca *x509.Certificate, required []string, now time.Time) bool {
	if now.Add(renewSoon).After(leaf.NotAfter) || leaf.CheckSignatureFrom(ca) != nil {
		return true
	}
	for _, h := range required {
		if leaf.VerifyHostname(h) != nil {
			return true
		}
	}
	return false
}

// ensureLocal loads the local CA and the server certificate from dir, creating or renewing
// them as needed. caNew reports a new CA: the only event clients have to act on.
func ensureLocal(dir string, hosts, required []string, now time.Time, logf func(string, ...any)) (pair tls.Certificate, caDER []byte, caNew bool, err error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return pair, nil, false, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		logf("[TLS] WARN cannot restrict %s to 0700: %v", dir, err) // the key files are 0600 anyway
	}
	caCert, caKey := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	ca, err := loadCA(caCert, caKey)
	if err != nil || now.Add(renewSoon).After(ca.cert.NotAfter) {
		name := "localhost"
		if h, herr := hostname(); herr == nil && h != "" {
			name = h
		}
		c, k, gerr := generateCA(name, now)
		if gerr != nil {
			return pair, nil, false, gerr
		}
		if werr := writePair(caCert, c, caKey, k); werr != nil {
			return pair, nil, false, werr
		}
		if ca, err = loadCA(caCert, caKey); err != nil {
			return pair, nil, false, err
		}
		caNew = true
	}
	leafCert, leafKey := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	old, oldErr := tls.LoadX509KeyPair(leafCert, leafKey)
	if !caNew && oldErr == nil && old.Leaf != nil && !needsRenewal(old.Leaf, ca.cert, required, now) {
		return old, ca.cert.Raw, false, nil
	}
	c, k, err := generateLeaf(ca, hosts, now)
	if err != nil {
		return pair, nil, false, err
	}
	if err := writePair(leafCert, c, leafKey, k); err != nil {
		// A read-only STATE must not cost a certificate that still works.
		if !caNew && oldErr == nil && old.Leaf != nil && now.Before(old.Leaf.NotAfter) && old.Leaf.CheckSignatureFrom(ca.cert) == nil {
			logf("[TLS] WARN cannot renew the certificate in %s: %v — keeping the current one", dir, err)
			return old, ca.cert.Raw, false, nil
		}
		return pair, nil, false, err
	}
	pair, err = tls.X509KeyPair(c, k)
	return pair, ca.cert.Raw, caNew, err
}

// writePair writes the key first: a crash in between leaves a pair that does not load,
// which is regenerated at the next start.
func writePair(certFile string, certPEM []byte, keyFile string, keyPEM []byte) error {
	if err := writeFile(keyFile, keyPEM); err != nil {
		return err
	}
	return writeFile(certFile, certPEM)
}

var writeFile = writeAtomic

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tls-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// A power cut on the Pi must not leave an empty file: that would mean a new CA.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	hexs := make([]string, len(sum))
	for i, b := range sum {
		hexs[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(hexs, ":")
}

// infoOf describes pair; with caDER the fingerprint shown is the CA's, the one clients trust.
func infoOf(pair *tls.Certificate, caDER []byte, source, path string) Info {
	leaf := pair.Leaf
	in := Info{Source: source, Fingerprint: fingerprint(pair.Certificate[0]), NotBefore: leaf.NotBefore,
		NotAfter: leaf.NotAfter, Path: path}
	if caDER != nil {
		in.Fingerprint = fingerprint(caDER)
		if ca, err := x509.ParseCertificate(caDER); err == nil {
			in.CANotAfter = ca.NotAfter
		}
	}
	in.SAN = slices.Clone(leaf.DNSNames)
	for _, ip := range leaf.IPAddresses {
		in.SAN = append(in.SAN, ip.String())
	}
	return in
}
