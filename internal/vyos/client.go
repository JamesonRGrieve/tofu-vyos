// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Package vyos is a minimal client for the VyOS HTTP API
// (https://docs.vyos.io/en/latest/automation/vyos-api.html).
//
// Unlike a REST CRUD API, VyOS is *config-path* based. Every request is an HTTP
// POST of multipart/form-data carrying two fields:
//
//   - data : a JSON document describing the operation — an object (or a list of
//     objects) of the form {"op": ..., "path": [...]}.
//   - key  : the plaintext API key configured under `service https api`.
//
// Endpoints used by this provider:
//
//   - /configure : op "set" / "delete" — mutates the config and commits the
//     change transactionally (a successful POST IS a commit).
//   - /retrieve  : op "showConfig" (subtree as JSON), "returnValues" (leaf
//     list), "exists" (bool) — read the running config at a path.
//   - /config-file : op "save" — persist the running config to /config/config.boot.
//
// Every endpoint returns the same envelope: {"success": bool, "data": ...,
// "error": string|null}. A non-2xx HTTP status or success=false is an error.
// This client is generic over the API: any config path is expressible.
package vyos

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Client is an API-key-authenticated VyOS HTTP API client. The key travels as a
// form field on every request; there is no session/cookie to establish, so the
// client is stateless and safe for concurrent use. Callers may share one Client
// across resources (the provider does).
type Client struct {
	base string // e.g. https://192.168.7.x  (no trailing slash, no path)
	key  string
	http *http.Client
}

// Config configures a Client.
type Config struct {
	// Host is the router address (host or host:port), no scheme. VyOS serves the
	// API over HTTPS; the port defaults to 443 unless included here.
	Host string
	// Key is the plaintext API key (service https api keys id <name> key <key>).
	Key string
	// Insecure skips TLS verification (VyOS ships a self-signed cert by default;
	// true is the norm on a lab / OOB management network).
	Insecure bool
	// Timeout per request (default 60s — a commit can take a few seconds).
	Timeout time.Duration
}

// NewClient builds a Client. It does not contact the router until the first
// API call.
func NewClient(c Config) *Client {
	if c.Timeout == 0 {
		c.Timeout = 60 * time.Second
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.Insecure}, //nolint:gosec // self-signed mgmt cert
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	host := strings.TrimSuffix(strings.TrimPrefix(c.Host, "https://"), "/")
	host = strings.TrimPrefix(host, "http://")
	return &Client{
		base: "https://" + host,
		key:  c.Key,
		http: &http.Client{Timeout: c.Timeout, Transport: tr},
	}
}

// Command is one VyOS API operation. VyOS expresses a config command as an op
// plus a path array; for set/delete the *value* is the final element of the
// path (e.g. set interfaces ethernet eth1 address 1.2.3.4/24 →
// path ["interfaces","ethernet","eth1","address","1.2.3.4/24"]). Callers
// therefore encode values as trailing path segments rather than a separate
// field, which keeps this client generic over every config path.
type Command struct {
	Op   string   `json:"op"`
	Path []string `json:"path,omitempty"`
}

// envelope is the universal VyOS API response shape.
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *string         `json:"error"`
}

// APIError is returned when VyOS responds with a non-2xx status or success=false.
type APIError struct {
	Endpoint string
	Status   int
	Message  string // the API `error` field, or raw body on a transport-level failure
}

func (e *APIError) Error() string {
	return fmt.Sprintf("vyos %s: HTTP %d: %s", e.Endpoint, e.Status, e.Message)
}

// NotFound reports whether err is an APIError whose message indicates the
// requested config path is absent/empty. VyOS has no 404; showConfig on a
// missing path returns success=false with an "empty"/"not exist"/"invalid"
// style message, which we normalize here so the resource Read can drop the
// object from state cleanly.
func NotFound(err error) bool {
	var ae *APIError
	if e, ok := err.(*APIError); ok {
		ae = e
	}
	if ae == nil {
		return false
	}
	m := strings.ToLower(ae.Message)
	return strings.Contains(m, "empty") ||
		strings.Contains(m, "is not valid") ||
		strings.Contains(m, "not exist") ||
		strings.Contains(m, "nonexistent") ||
		strings.Contains(m, "does not exist")
}

// post issues a single form-encoded request to the given endpoint with the
// given data payload (already JSON-marshaled) and returns the decoded data
// field on success.
func (c *Client) post(endpoint string, data []byte) (json.RawMessage, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("data", string(data)); err != nil {
		return nil, err
	}
	if err := mw.WriteField("key", c.key); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.base+endpoint, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vyos %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var env envelope
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		// Non-JSON body (e.g. an auth/HTML error page) — surface it raw.
		return nil, &APIError{Endpoint: endpoint, Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	}
	if resp.StatusCode/100 != 2 || !env.Success {
		msg := ""
		if env.Error != nil {
			msg = *env.Error
		}
		return nil, &APIError{Endpoint: endpoint, Status: resp.StatusCode, Message: msg}
	}
	return env.Data, nil
}

// Configure POSTs one or more set/delete commands to /configure. A successful
// response means the change was applied AND committed (VyOS commits the session
// transactionally). Passing >1 command sends them as a JSON list so they commit
// atomically.
func (c *Client) Configure(cmds []Command) error {
	if len(cmds) == 0 {
		return nil
	}
	var data []byte
	var err error
	if len(cmds) == 1 {
		data, err = json.Marshal(cmds[0])
	} else {
		data, err = json.Marshal(cmds)
	}
	if err != nil {
		return err
	}
	_, err = c.post("/configure", data)
	return err
}

// ShowConfig returns the running-config subtree at path as raw JSON (the
// envelope `data` field). An empty path returns the entire configuration. A
// NotFound error is returned when the path is absent.
func (c *Client) ShowConfig(path []string) (json.RawMessage, error) {
	data, err := json.Marshal(Command{Op: "showConfig", Path: path})
	if err != nil {
		return nil, err
	}
	return c.post("/retrieve", data)
}

// Exists reports whether the given config path is present (op "exists").
func (c *Client) Exists(path []string) (bool, error) {
	data, err := json.Marshal(Command{Op: "exists", Path: path})
	if err != nil {
		return false, err
	}
	raw, err := c.post("/retrieve", data)
	if err != nil {
		return false, err
	}
	var b bool
	if uerr := json.Unmarshal(raw, &b); uerr != nil {
		return false, fmt.Errorf("vyos /retrieve exists: unexpected data: %s", string(raw))
	}
	return b, nil
}

// Save persists the running config to /config/config.boot (op "save").
func (c *Client) Save() error {
	data, err := json.Marshal(Command{Op: "save"})
	if err != nil {
		return err
	}
	_, err = c.post("/config-file", data)
	return err
}
