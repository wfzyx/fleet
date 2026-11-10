// Package integrations contains clients for the external systems fleet talks
// to: GitHub (pull requests and issues) and Jira (issues and transitions).
package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxResponseBytes caps how much of a response body is read.
const maxResponseBytes = 16 << 20

// Sentinel errors matched with errors.Is against *APIError.
var (
	// ErrNotFound is returned for HTTP 404.
	ErrNotFound = errors.New("integrations: not found")
	// ErrUnauthorized is returned for HTTP 401 (missing, expired or invalid credentials).
	ErrUnauthorized = errors.New("integrations: unauthorized")
	// ErrNoCredentials is returned when no credentials could be found.
	ErrNoCredentials = errors.New("integrations: no credentials found")
)

// APIError is a non-success HTTP response from a remote API. It never
// contains request headers, so credentials cannot leak through it.
type APIError struct {
	Service string // "github" or "jira"
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%s: %s %s: HTTP %d: %s", e.Service, e.Method, e.Path, e.Status, msg)
}

// Is maps status codes onto the package sentinels.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	}
	return false
}

// response is a fully read HTTP response.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

// send performs req and reads at most maxResponseBytes of the body.
func send(hc *http.Client, req *http.Request) (*response, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", req.Method, req.URL.Path, err)
	}
	return &response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// newJSONRequest builds a request with an optional JSON body.
func newJSONRequest(ctx context.Context, method, url string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// decode unmarshals body into out when out is non-nil and body is non-empty.
func decode(body []byte, out any) error {
	if out == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func hasStatus(status int, ok []int) bool {
	for _, s := range ok {
		if s == status {
			return true
		}
	}
	return false
}
