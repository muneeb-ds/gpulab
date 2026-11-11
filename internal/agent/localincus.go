package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// LocalIncus is what the agent needs from the Incus daemon on its own host.
type LocalIncus interface {
	// Fingerprint of the Incus server certificate, so the control plane can pin it.
	Fingerprint() (string, error)
	// TrustCert adds the control plane's client certificate to Incus's trust store.
	TrustCert(name, certPEM string) error
}

// UnixIncus talks to incusd over its local unix socket (root only).
type UnixIncus struct{ c *http.Client }

func NewUnixIncus(socket string) *UnixIncus {
	return &UnixIncus{c: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

func (u *UnixIncus) call(method, path string, body any) (json.RawMessage, error) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, "http://incus"+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Type     string          `json:"type"`
		Error    string          `json:"error"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if r.Type == "error" {
		return nil, fmt.Errorf("%s", r.Error)
	}
	return r.Metadata, nil
}

func (u *UnixIncus) Fingerprint() (string, error) {
	m, err := u.call("GET", "/1.0", nil)
	if err != nil {
		return "", err
	}
	var srv struct {
		Environment struct {
			Fingerprint string `json:"certificate_fingerprint"`
		} `json:"environment"`
	}
	if err := json.Unmarshal(m, &srv); err != nil {
		return "", err
	}
	return srv.Environment.Fingerprint, nil
}

func (u *UnixIncus) TrustCert(name, certPEM string) error {
	blk, _ := pem.Decode([]byte(certPEM))
	if blk == nil {
		return fmt.Errorf("invalid certificate PEM")
	}
	_, err := u.call("POST", "/1.0/certificates", map[string]any{
		"name": name, "type": "client", "certificate": base64.StdEncoding.EncodeToString(blk.Bytes),
	})
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already") {
		return nil
	}
	return err
}
