// Package zrepl installs and configures zrepl on a host from EZDR's desired
// state. See docs/design/replication.md.
package zrepl

import "path/filepath"

// Paths are the files and directories the package manages. Tests use a
// temporary root.
type Paths struct {
	// MainConfig is zrepl's main configuration file.
	MainConfig string
	// JobsDir is included by the main configuration; EZDR's jobs go in
	// JobsDir/ezdr.yml.
	JobsDir string
	// CertDir holds this host's zrepl TLS key and certificate, and PeersDir
	// the peers' certificates.
	CertDir  string
	PeersDir string
}

// DefaultPaths are the real locations on a Proxmox VE host.
var DefaultPaths = Paths{
	MainConfig: "/etc/zrepl/zrepl.yml",
	JobsDir:    "/etc/zrepl/ezdr.d",
	CertDir:    "/etc/ezdr/zrepl",
	PeersDir:   "/etc/ezdr/zrepl/peers",
}

// JobsFile is EZDR's job file.
func (p Paths) JobsFile() string { return filepath.Join(p.JobsDir, "ezdr.yml") }

// CertFile and KeyFile are this host's zrepl TLS certificate and key.
func (p Paths) CertFile() string { return filepath.Join(p.CertDir, "cert.pem") }

// KeyFile is this host's zrepl TLS private key.
func (p Paths) KeyFile() string { return filepath.Join(p.CertDir, "key.pem") }

// PeerFile is where a peer's certificate is stored.
func (p Paths) PeerFile(peer string) string { return filepath.Join(p.PeersDir, peer+".pem") }
