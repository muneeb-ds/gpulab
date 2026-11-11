// Package incus is a minimal Incus REST client: just the calls the control
// plane needs, over mTLS with the server certificate pinned by fingerprint.
package incus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrNotFound = errors.New("incus: not found")

type Instance struct {
	Name         string                       `json:"name"`
	Status       string                       `json:"status,omitempty"` // Running, Stopped, ...
	Architecture string                       `json:"architecture,omitempty"`
	Config       map[string]string            `json:"config"`
	Devices      map[string]map[string]string `json:"devices"`
	Profiles     []string                     `json:"profiles"`
	Ephemeral    bool                         `json:"ephemeral"`
	Stateful     bool                         `json:"stateful"`
	Description  string                       `json:"description"`
}

type InstanceSource struct {
	Type     string `json:"type"` // image
	Alias    string `json:"alias,omitempty"`
	Server   string `json:"server,omitempty"`
	Protocol string `json:"protocol,omitempty"` // simplestreams
}

type InstancesPost struct {
	Instance
	Source InstanceSource `json:"source"`
	Type   string         `json:"type"` // container
}

// Client is the subset of Incus the control plane uses.
type Client interface {
	CreateInstance(ctx context.Context, req InstancesPost) error
	GetInstance(ctx context.Context, name string) (*Instance, string, error)
	UpdateInstance(ctx context.Context, name string, inst Instance, etag string) error
	SetState(ctx context.Context, name, action string, force bool) error
	DeleteInstance(ctx context.Context, name string) error
}

type client struct {
	base string
	http *http.Client
}

// Fingerprint returns the Incus-style fingerprint (sha256 of the DER) of a cert.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// New returns a client for an Incus server reachable at baseURL whose
// certificate has the given fingerprint.
func New(baseURL, fingerprint string, cert tls.Certificate) Client {
	fp := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Incus servers use self-signed certificates; we pin instead of
		// relying on a CA. VerifyPeerCertificate enforces the pin.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || fp == "" || Fingerprint(raw[0]) != fp {
				return fmt.Errorf("incus server certificate does not match pinned fingerprint")
			}
			return nil
		},
		MinVersion: tls.VersionTLS12,
	}
	return &client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: 15 * time.Minute, Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	}
}

type response struct {
	Type       string          `json:"type"` // sync|async|error
	StatusCode int             `json:"status_code"`
	Operation  string          `json:"operation"`
	ErrorCode  int             `json:"error_code"`
	Error      string          `json:"error"`
	Metadata   json.RawMessage `json:"metadata"`
}

func (c *client) do(ctx context.Context, method, path string, body any, etag string) (*response, string, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var r response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, "", fmt.Errorf("incus %s %s: bad response (HTTP %d): %w", method, path, resp.StatusCode, err)
	}
	if r.Type == "error" || resp.StatusCode >= 400 {
		if r.ErrorCode == 404 || resp.StatusCode == 404 {
			return nil, "", ErrNotFound
		}
		return nil, "", fmt.Errorf("incus %s %s: %s", method, path, r.Error)
	}
	return &r, resp.Header.Get("ETag"), nil
}

// wait blocks until an async operation finishes.
func (c *client) wait(ctx context.Context, r *response) error {
	if r.Type != "async" || r.Operation == "" {
		return nil
	}
	wr, _, err := c.do(ctx, "GET", r.Operation+"/wait?timeout=900", nil, "")
	if err != nil {
		return err
	}
	var op struct {
		StatusCode int    `json:"status_code"`
		Status     string `json:"status"`
		Err        string `json:"err"`
	}
	if err := json.Unmarshal(wr.Metadata, &op); err != nil {
		return err
	}
	if op.StatusCode != 200 {
		if op.Err == "" {
			op.Err = op.Status
		}
		return fmt.Errorf("incus operation failed: %s", op.Err)
	}
	return nil
}

func (c *client) run(ctx context.Context, method, path string, body any, etag string) error {
	r, _, err := c.do(ctx, method, path, body, etag)
	if err != nil {
		return err
	}
	return c.wait(ctx, r)
}

func (c *client) CreateInstance(ctx context.Context, req InstancesPost) error {
	return c.run(ctx, "POST", "/1.0/instances", req, "")
}

func (c *client) GetInstance(ctx context.Context, name string) (*Instance, string, error) {
	r, etag, err := c.do(ctx, "GET", "/1.0/instances/"+url.PathEscape(name), nil, "")
	if err != nil {
		return nil, "", err
	}
	var inst Instance
	if err := json.Unmarshal(r.Metadata, &inst); err != nil {
		return nil, "", err
	}
	return &inst, etag, nil
}

func (c *client) UpdateInstance(ctx context.Context, name string, inst Instance, etag string) error {
	put := map[string]any{
		"architecture": inst.Architecture, "config": inst.Config, "devices": inst.Devices,
		"profiles": inst.Profiles, "ephemeral": inst.Ephemeral, "stateful": inst.Stateful, "description": inst.Description,
	}
	return c.run(ctx, "PUT", "/1.0/instances/"+url.PathEscape(name), put, etag)
}

func (c *client) SetState(ctx context.Context, name, action string, force bool) error {
	return c.run(ctx, "PUT", "/1.0/instances/"+url.PathEscape(name)+"/state",
		map[string]any{"action": action, "force": force, "timeout": 60}, "")
}

func (c *client) DeleteInstance(ctx context.Context, name string) error {
	return c.run(ctx, "DELETE", "/1.0/instances/"+url.PathEscape(name), nil, "")
}
