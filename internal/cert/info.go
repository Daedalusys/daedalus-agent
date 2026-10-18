// Package cert — Info type + LoaderInfo() helper.
//
// The agent's Register response surfaces two pieces of metadata
// from the certificate: the cert's NotAfter (Unix seconds), and
// its SHA-256 fingerprint (the control plane uses that to detect
// "agent presented a different cert than the one we issued" —
// i.e. CA compromise or re-install).
//
// We expose this through a small Info struct rather than letting
// callers reach into the loader's internals. The fingerprint is
// computed at Info() time from the on-disk leaf cert; the
// NotAfter is read off the same parsed cert. Both are stable
// across Load() calls until rotation fires.
package cert

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// Info summarises the metadata the agent's gRPC layer surfaces
// about the server certificate to the control plane.
//
// All fields are read-only; the Info is a snapshot of cert state
// at the moment LoaderInfo() was called. The gRPC layer caches
// the Info at NewServer time and re-reads it on rotation, which
// is rare enough that callers do not need to worry about it.
type Info struct {
	// ValidUntil is the cert's NotAfter, as a UTC time. The
	// gRPC layer converts this to Unix seconds for Register.
	ValidUntil time.Time

	// Fingerprint is the SHA-256 of the cert's DER encoding,
	// colon-separated in the canonical "xx:xx:…:xx" form. The
	// control plane stores this as the canonical identifier
	// of "which key signed this cert" — when an agent
	// re-presents after a rotation, the fingerprint changes
	// and the control plane knows to re-validate.
	Fingerprint string
}

// LoaderInfo reads the cert material that loader is configured
// against and returns an Info summarising it. It is the
// production-side counterpart to TestLoaderInfo (below) —
// production code calls LoaderInfo(loader), test code passes a
// pre-built Info so the tests can avoid touching disk.
//
// We re-parse the cert from disk rather than reading it from the
// loader's last Load()'s *tls.Config because:
//
//  1. Load() returns a *tls.Config which is opaque to most of its
//     fields (the leaf cert is wrapped in a tls.Certificate).
//  2. Re-parsing from disk is one read per Register — not in
//     the hot path; Register is called once per cert rotation,
//     not per RPC.
func LoaderInfo(loader *Loader) (*Info, error) {
	if loader == nil {
		return nil, fmt.Errorf("cert: LoaderInfo: nil loader")
	}
	leafPEM, err := os.ReadFile(loader.cfg.CertPath)
	if err != nil {
		return nil, fmt.Errorf("cert: read %s: %w", loader.cfg.CertPath, err)
	}
	return parseInfoFromPEM(leafPEM)
}

// parseInfoFromPEM is the helper LoaderInfo delegates to; it
// splits the PEM into a leaf certificate, validates it's not
// empty, and returns an Info.
func parseInfoFromPEM(leafPEM []byte) (*Info, error) {
	if len(leafPEM) == 0 {
		return nil, fmt.Errorf("cert: empty leaf PEM")
	}
	cert, err := parseCert(leafPEM)
	if err != nil {
		return nil, fmt.Errorf("cert: parse leaf: %w", err)
	}
	return infoFromCert(cert), nil
}

// infoFromCert extracts the ValidUntil + Fingerprint fields from
// an already-parsed leaf cert. Splitting parse → extract lets
// tests construct an Info from a synthesised cert without
// round-tripping through PEM.
func infoFromCert(cert *x509.Certificate) *Info {
	fp := sha256.Sum256(cert.Raw)
	return &Info{
		ValidUntil:  cert.NotAfter.UTC(),
		Fingerprint: hex.EncodeToString(fp[:]),
	}
}