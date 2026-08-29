package netpolicy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// HTTPS inspection: the certificate authority half.
//
// ADR-0014 forbade this outright and ADR-0023 narrowed that ban rather than
// lifting it. Read ADR-0023 before changing anything here; the short version
// is that the ban's stated reason — "the proxy would become the highest
// secret concentration in the system" — is about what the proxy RECORDS, not
// about whether it can decrypt. So: decryption is opt-in and off by default,
// and nothing in this package writes a request body, a header, or a URL path
// anywhere. The audit line carries host and method only.

// caCertFile and caKeyFile are the on-disk names inside the CA directory. They
// are fixed rather than configurable because the child-facing environment
// variables have to name the same file, and two ways to spell it is one way
// for them to disagree.
const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca-key.pem"

	// caValidity is how long a generated root lives. A year is short enough
	// that a leaked root expires on a human timescale and long enough that
	// rotation is not a weekly chore. An expired root regenerates on the next
	// start; see LoadOrCreateCA.
	caValidity = 365 * 24 * time.Hour
	// leafValidity bounds a minted host certificate. Leaves are in-memory
	// only, so this mostly guards a long-running process against handing out a
	// certificate whose notAfter has passed.
	leafValidity = 48 * time.Hour
)

// CertAuthority mints per-host certificates for the inspecting proxy.
//
// # Where the private key lives
//
// dir, which bootstrap sets to ~/.yanshi/tls. The directory is created 0700
// and the key file 0600, and both are re-checked on load: a key that another
// user can read is refused rather than used, because a readable MITM root is a
// standing authority to impersonate every site the operator visits, not just
// the ones yanshi proxies.
//
// # What trusts it
//
// Nothing, by default. It is not installed into any system or browser trust
// store — doing that would extend the forgery power to the operator's whole
// machine to buy request inspection inside yanshi's own children. Only
// processes yanshi launches trust it, and only because CAEnv puts the
// certificate path in their environment (SSL_CERT_FILE and friends). A program
// that ignores those variables, or that pins certificates, does not trust it
// and its CONNECT fails the handshake — visibly, not silently.
type CertAuthority struct {
	dir  string
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// pemPath is the certificate path handed to children. Kept as a field so
	// callers do not re-derive the join and drift from the loader.
	pemPath string

	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// LoadOrCreateCA loads the root from dir, generating one if it is absent,
// unreadable as a pair, or already expired.
//
// Regenerating on expiry rather than erroring is deliberate: the root is a
// local artefact with no external trust to preserve, so the alternative is a
// proxy that refuses to start a year after first use with an error the
// operator has to research. Children pick up the new root through the same
// environment variables on their next launch.
func LoadOrCreateCA(dir string) (*CertAuthority, error) {
	if dir == "" {
		return nil, fmt.Errorf("netpolicy: CA directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("netpolicy: ca dir: %w", err)
	}
	ca := &CertAuthority{dir: dir, pemPath: filepath.Join(dir, caCertFile), leafs: map[string]*tls.Certificate{}}
	if err := ca.load(); err == nil {
		return ca, nil
	}
	if err := ca.generate(); err != nil {
		return nil, err
	}
	return ca, nil
}

// CertPath is the PEM file children are pointed at. Callers must not assume it
// exists before LoadOrCreateCA returned successfully.
func (c *CertAuthority) CertPath() string { return c.pemPath }

// load reads an existing pair and rejects it unless it is usable AND the key
// file is private. The permission check is skipped on Windows, where the file
// mode Go reports is synthesised and says nothing about the real ACL.
func (c *CertAuthority) load() error {
	keyPath := filepath.Join(c.dir, caKeyFile)
	info, err := os.Stat(keyPath)
	if err != nil {
		return err
	}
	if err := checkKeyPerm(info); err != nil {
		return err
	}
	certPEM, err := os.ReadFile(c.pemPath)
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return fmt.Errorf("netpolicy: ca files are not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return err
	}
	if time.Now().After(cert.NotAfter) {
		return fmt.Errorf("netpolicy: ca expired at %s", cert.NotAfter)
	}
	c.cert, c.key = cert, key
	return nil
}

// generate mints a fresh root and writes it out. The key is written before the
// certificate so a crash between the two leaves a pair that load() rejects
// (missing cert) rather than a certificate with no key.
func (c *CertAuthority) generate() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "yanshi managed proxy root", Organization: []string{"yanshi"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPath := filepath.Join(c.dir, caKeyFile)
	// Remove first. os.WriteFile's mode applies only when it CREATES the file,
	// so writing over an existing key leaves whatever permissions it had — and
	// the single most important reason to be regenerating is that load()
	// rejected the old key for being world-readable. Without this the "fix"
	// wrote a fresh private key into the same readable file, every start,
	// forever. Found by TestCertAuthorityRegeneratesWhenTheKeyIsWorldReadable.
	if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(c.pemPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	c.cert, c.key = cert, key
	return nil
}

// LeafFor returns the certificate to present for one ClientHello, minting and
// caching it on first use for that server name.
//
// A ClientHello with no SNI cannot be answered honestly — there is no name to
// put in the certificate — so it is refused rather than served with a
// wildcard. The child sees a handshake failure, which is the correct signal:
// the proxy could not identify what it was being asked to impersonate.
func (c *CertAuthority) LeafFor(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello == nil || hello.ServerName == "" {
		return nil, fmt.Errorf("netpolicy: TLS ClientHello carried no server name")
	}
	return c.leafForHost(hello.ServerName)
}

// leafForHost is LeafFor keyed by a plain host string, split out so the CONNECT
// path can pre-mint and so tests do not have to fabricate a ClientHello.
func (c *CertAuthority) leafForHost(host string) (*tls.Certificate, error) {
	host = normalizeHost(host)
	if host == "" {
		return nil, fmt.Errorf("netpolicy: empty host")
	}
	c.mu.Lock()
	if leaf, ok := c.leafs[host]; ok {
		c.mu.Unlock()
		return leaf, nil
	}
	c.mu.Unlock()

	leaf, err := c.mint(host)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	// Another goroutine may have minted the same host while this one was
	// signing. Keep the first: two valid certificates are interchangeable, and
	// overwriting would leave a handshake in flight holding one that is no
	// longer the cached answer.
	if existing, ok := c.leafs[host]; ok {
		leaf = existing
	} else {
		c.leafs[host] = leaf
	}
	c.mu.Unlock()
	return leaf, nil
}

// mint signs one host certificate. An IP literal goes in IPAddresses rather
// than DNSNames; a certificate that carries an IP as a DNS name validates
// nowhere.
func (c *CertAuthority) mint(host string) (*tls.Certificate, error) {
	if c.cert == nil || c.key == nil {
		return nil, fmt.Errorf("netpolicy: certificate authority is not initialised")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  key,
		Leaf:        tmpl,
	}, nil
}

// checkKeyPerm refuses a root key any other user can read.
//
// Skipped on Windows, where os.FileInfo.Mode() for a regular file is
// synthesised from the read-only attribute and carries no information about
// the ACL that actually governs access. Asserting 0600 there would either
// always fail or always pass depending on that attribute, and neither answer
// is about who can read the key.
func checkKeyPerm(info os.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("netpolicy: ca key is mode %04o; it must not be readable by group or other", mode)
	}
	return nil
}

// randomSerial draws a 128-bit positive serial. Sequential serials would leak
// how many certificates the proxy has issued into every certificate it issues.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

// caTrustEnvKeys are the environment variables that make a child trust the
// generated root. Each names a real consumer rather than being a hopeful
// spread:
//
//	SSL_CERT_FILE      OpenSSL, and therefore curl, git and most C clients
//	CURL_CA_BUNDLE     curl's own override, which wins over SSL_CERT_FILE
//	REQUESTS_CA_BUNDLE python-requests
//	NODE_EXTRA_CA_CERTS node (ADDS to the built-in store rather than replacing)
//	GIT_SSL_CAINFO     git's HTTPS transport when built against OpenSSL
//
// Go programs are absent on purpose: Go's crypto/tls reads SSL_CERT_FILE on
// unix, so they are covered by the first entry, and there is no Windows
// equivalent to publish. A Go child on Windows will NOT trust the root — see
// ADR-0023's platform table.
var caTrustEnvKeys = []string{
	"SSL_CERT_FILE",
	"CURL_CA_BUNDLE",
	"REQUESTS_CA_BUNDLE",
	"NODE_EXTRA_CA_CERTS",
	"GIT_SSL_CAINFO",
}

// CAEnv returns the KEY=VALUE entries that point a child at certPath, or nil
// when certPath is empty.
//
// Returning nil for the empty case is what keeps inspection opt-in end to end:
// with inspection off there is no CA, no path, and therefore not one
// certificate variable in any child environment — the child's trust store is
// exactly what it would have been with this feature absent.
func CAEnv(certPath string) []string {
	if certPath == "" {
		return nil
	}
	out := make([]string, 0, len(caTrustEnvKeys))
	for _, key := range caTrustEnvKeys {
		out = append(out, key+"="+certPath)
	}
	return out
}
