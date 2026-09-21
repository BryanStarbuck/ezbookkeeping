// Package client is the CLI's ONE HTTP client for the machine plane (cli.mdx §5). It is the only
// package that opens a socket, and it never sends the API secret key to a URL that is neither
// loopback nor https (R3).
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/exitcode"
)

// Version is the CLI version sent as X-Ezbk-Client
var Version = "0.1.0"

// BasePath is where the machine plane lives on the server
const BasePath = "/machine/v1"

// Client talks to one install's machine plane
type Client struct {
	BaseURL  string // e.g. http://127.0.0.1:8080
	Key      string
	Timezone string
	Timeout  time.Duration // 0 = no client-side timeout (the long verbs, cli.mdx §7.4)
	HTTP     *http.Client
}

// APIError is a machine-plane error envelope
type APIError struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	Hint         string `json:"hint"`
	Details      any    `json:"details,omitempty"`
	UpstreamCode int    `json:"upstreamCode,omitempty"`
	Status       int    `json:"-"`
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return e.Code
	}

	return e.Message
}

// ExitCode maps a plane error code to the CLI exit-code contract
func (e *APIError) ExitCode() int {
	switch e.Code {
	case "unauthorized":
		return exitcode.Unauthorized
	case "not_found":
		return exitcode.NotFound
	case "conflict":
		return exitcode.Conflict
	case "write_disabled":
		return exitcode.TierRefused
	case "forbidden":
		if strings.Contains(e.Message, "tier") {
			return exitcode.TierRefused
		}

		return exitcode.Failed
	case "not_ready":
		if strings.Contains(strings.ToLower(e.Message), "user") {
			return exitcode.NotFound
		}

		return exitcode.Unreachable
	case "invalid_input":
		return exitcode.Usage
	}

	return exitcode.Failed
}

// Envelope is one machine-plane response
type Envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Meta  map[string]any  `json:"meta,omitempty"`
	Error *APIError       `json:"error,omitempty"`
	Raw   []byte          `json:"-"`
}

// Decode unmarshals the envelope's data into out, keeping numbers exact
func (e *Envelope) Decode(out any) error {
	if len(e.Data) == 0 {
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(e.Data))
	dec.UseNumber()

	return dec.Decode(out)
}

// DataMap returns the data object as a generic map
func (e *Envelope) DataMap() map[string]any {
	var m map[string]any

	_ = e.Decode(&m)

	return m
}

// ErrUnreachable means no socket could be opened to the server
var ErrUnreachable = errors.New("the machine plane could not be reached")

// IsLoopbackURL reports whether a base URL points at this machine
func IsLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)

	if err != nil {
		errfile.Expected("parsing the base URL to test for loopback", err)
		return false
	}

	host := u.Hostname()

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// CheckTarget refuses to attach the key to a non-loopback plain-http URL (R3)
func CheckTarget(raw string) error {
	u, err := url.Parse(raw)

	if err != nil || u.Scheme == "" || u.Host == "" {
		errfile.Expected("parsing the target URL", err)
		return fmt.Errorf("%q is not a URL like http://127.0.0.1:8080", raw)
	}

	if !IsLoopbackURL(raw) && u.Scheme != "https" {
		return fmt.Errorf("refusing to send the API secret key to %s: only loopback or https targets are allowed", raw)
	}

	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}

	return &http.Client{Timeout: c.Timeout}
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	if err := CheckTarget(c.BaseURL); err != nil {
		return nil, err
	}

	target := strings.TrimRight(c.BaseURL, "/") + BasePath + path

	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	contentType := ""

	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
		contentType = "application/json"
	case *MultipartBody:
		reader = b.Body
		contentType = b.ContentType
	default:
		data, err := json.Marshal(body)

		if err != nil {
			return nil, err
		}

		reader = bytes.NewReader(data)
		contentType = "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)

	if err != nil {
		return nil, err
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Ezbk-Api-Key", c.Key)
	req.Header.Set("X-Ezbk-Client", "ezbk/"+Version)

	if c.Timezone != "" {
		req.Header.Set("X-Timezone-Name", c.Timezone)
	}

	return req, nil
}

// MultipartBody carries a prebuilt multipart form
type MultipartBody struct {
	Body        io.Reader
	ContentType string
}

// Do performs one call and decodes the envelope. A plane error is returned as *APIError alongside
// the envelope; a transport failure wraps ErrUnreachable.
func (c *Client) Do(method, path string, query url.Values, body any) (*Envelope, error) {
	req, err := c.newRequest(context.Background(), method, path, query, body)

	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	return parseEnvelope(resp, data)
}

func parseEnvelope(resp *http.Response, data []byte) (*Envelope, error) {
	ct := resp.Header.Get("Content-Type")

	if !strings.Contains(ct, "json") {
		if resp.StatusCode >= 400 {
			return nil, &APIError{Code: "internal", Message: fmt.Sprintf("HTTP %d", resp.StatusCode), Status: resp.StatusCode}
		}

		return &Envelope{OK: true, Raw: data}, nil
	}

	env := &Envelope{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	if err := dec.Decode(env); err != nil {
		errfile.Caught("decoding the machine-plane envelope", err, errfile.F("http_status", resp.StatusCode))
		return nil, fmt.Errorf("the server answered with something that is not the machine-plane envelope (HTTP %d)", resp.StatusCode)
	}

	env.Raw = data

	if !env.OK {
		if env.Error == nil {
			env.Error = &APIError{Code: "internal"}
		}

		env.Error.Status = resp.StatusCode

		return env, env.Error
	}

	return env, nil
}

// DoStream performs a call that may stream NDJSON progress lines (long ingest routes). onProgress
// receives every line but the last; the last line is the envelope.
func (c *Client) DoStream(method, path string, query url.Values, body any, onProgress func(map[string]any)) (*Envelope, error) {
	req, err := c.newRequest(context.Background(), method, path, query, body)

	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/x-ndjson")

	resp, err := c.httpClient().Do(req)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "ndjson") {
		data, err := io.ReadAll(resp.Body)

		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
		}

		return parseEnvelope(resp, data)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var last []byte

	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)

		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		if last != nil && onProgress != nil {
			var m map[string]any

			if json.Unmarshal(last, &m) == nil {
				onProgress(m)
			}
		}

		last = line
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	fake := &http.Response{StatusCode: resp.StatusCode, Header: http.Header{"Content-Type": {"application/json"}}}

	return parseEnvelope(fake, last)
}

// Healthz probes upstream's unauthenticated /healthz.json (the bring-up gate, cli.mdx §3.2)
func Healthz(baseURL string, timeout time.Duration) (map[string]any, error) {
	hc := &http.Client{Timeout: timeout}
	resp, err := hc.Get(strings.TrimRight(baseURL, "/") + "/healthz.json")

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	var m map[string]any

	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		errfile.Caught("decoding healthz.json", err)
		return nil, fmt.Errorf("healthz.json did not answer JSON")
	}

	// upstream wraps the payload as {"result":{"status":"ok",...},"success":true};
	// lift it so callers read status/version/commit at the top level
	if inner, ok := m["result"].(map[string]any); ok {
		for k, v := range inner {
			if _, exists := m[k]; !exists {
				m[k] = v
			}
		}
	}

	return m, nil
}
