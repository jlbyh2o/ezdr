// Package pve reads from the local Proxmox VE API with EZDR's read-only API
// token, and manages that token.
package pve

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Token identity and storage. See docs/design/inventory.md.
const (
	User      = "ezdr@pve"
	TokenName = "inventory"
	TokenID   = User + "!" + TokenName
	Role      = "PVEAuditor"
	// TokenFile holds "<token ID>=<secret>", readable only by root.
	TokenFile = "/etc/ezdr/pve-token" //nolint:gosec // a file path, not a credential
)

// baseURL is the local Proxmox VE API.
const baseURL = "https://127.0.0.1:8006/api2/json/"

// certFiles are the node's own certificates; the local API must present one
// of them exactly.
var certFiles = []string{"/etc/pve/local/pveproxy-ssl.pem", "/etc/pve/local/pve-ssl.pem"}

// Client calls the local Proxmox VE API.
type Client struct {
	http  *http.Client
	auth  string
	base  string
	certs [][]byte
}

// NewClient returns a client using the token in TokenFile.
func NewClient() (*Client, error) {
	b, err := os.ReadFile(TokenFile)
	if err != nil {
		return nil, err
	}
	var certs [][]byte
	for _, f := range certFiles {
		if der, err := readCertDER(f); err == nil {
			certs = append(certs, der)
		}
	}
	if len(certs) == 0 {
		return nil, errors.New("no Proxmox VE certificate found in /etc/pve/local")
	}
	return newClient(baseURL, strings.TrimSpace(string(b)), certs), nil
}

// newClient returns a client for base that authenticates with token and
// accepts only the given DER-encoded certificates.
func newClient(base, token string, certs [][]byte) *Client {
	c := &Client{auth: "PVEAPIToken=" + token, base: base, certs: certs}
	c.http = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Verification is an exact match against the node's own
			// certificate files, which works for default and custom
			// certificates without trusting anything else.
			// VerifyConnection (unlike VerifyPeerCertificate) also runs on
			// resumed sessions.
			InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection
			VerifyConnection: func(cs tls.ConnectionState) error {
				return c.verify(cs.PeerCertificates)
			},
		}},
	}
	return c
}

func (c *Client) verify(peer []*x509.Certificate) error {
	if len(peer) == 0 {
		return errors.New("no certificate presented")
	}
	for _, der := range c.certs {
		if bytes.Equal(peer[0].Raw, der) {
			return nil
		}
	}
	return errors.New("local Proxmox VE API presented an unexpected certificate")
}

func readCertDER(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // fixed paths
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no certificate", path)
	}
	return block.Bytes, nil
}

// Get calls GET path (relative to /api2/json/) and decodes the "data" field
// into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return json.Unmarshal(envelope.Data, out)
}
