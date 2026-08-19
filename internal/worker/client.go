package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes must comfortably exceed the server's 64KB request cap:
// queued tasks are re-marshaled with Go's JSON HTML escaping, which inflates
// '<', '>', and '&' six-fold before the payload reaches the lease response.
const maxResponseBytes = 512 << 10

// Lease is a capture job temporarily assigned to this worker.
type Lease struct {
	Job
	LeaseToken string `json:"leaseToken"`
	Attempts   int    `json:"attempts"`
}

// Client talks to the worker-only HTTPS API exposed by the server.
type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

type ClientConfig struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

func NewClient(config ClientConfig) (*Client, error) {
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse worker server URL: %w", err)
	}
	if baseURL.Scheme != "https" && !(baseURL.Scheme == "http" && isLiteralLoopback(baseURL.Hostname())) {
		return nil, errors.New("worker server URL must use HTTPS, except for HTTP on a literal loopback IP")
	}
	if baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("worker server URL must contain only a scheme, host, and optional base path")
	}
	if len(config.Token) < 32 {
		return nil, errors.New("worker token must be at least 32 characters")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: 40 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Client{baseURL: baseURL, token: config.Token, httpClient: client}, nil
}

func isLiteralLoopback(host string) bool {
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

// ErrUnauthorized reports that the server rejected the worker token.
var ErrUnauthorized = errors.New("the server rejected the worker token")

// Ping verifies connectivity and the worker token against the server's
// authenticated worker API without touching the queue.
func (c *Client) Ping(ctx context.Context) error {
	request, err := c.newRequest(ctx, http.MethodGet, "/worker/v1/ping", nil)
	if err != nil {
		return err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("ping worker API: %w", err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	}
	return responseError("ping worker API", response)
}

// Lease waits for the server to assign a job. A nil lease means the long poll
// completed normally without work.
func (c *Client) Lease(ctx context.Context) (*Lease, error) {
	request, err := c.newRequest(ctx, http.MethodPost, "/worker/v1/lease", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("lease capture job: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, responseError("lease capture job", response)
	}
	var lease Lease
	if err := decodeJSON(response.Body, &lease); err != nil {
		return nil, fmt.Errorf("decode leased capture job: %w", err)
	}
	if lease.ID == "" || lease.LeaseToken == "" {
		return nil, errors.New("leased capture job omitted its identifiers")
	}
	return &lease, nil
}

func (c *Client) Complete(ctx context.Context, lease Lease, outcome Outcome) error {
	body := struct {
		LeaseToken string   `json:"leaseToken"`
		ThingsID   string   `json:"thingsId"`
		Warnings   []string `json:"warnings,omitempty"`
	}{lease.LeaseToken, outcome.ThingsID, outcome.Warnings}
	return c.postJobResult(ctx, lease.ID, "complete", body)
}

func (c *Client) Fail(ctx context.Context, lease Lease, processErr error, retryable bool) error {
	if processErr == nil {
		return errors.New("worker failure requires an error")
	}
	body := struct {
		LeaseToken string `json:"leaseToken"`
		Error      string `json:"error"`
		Retryable  bool   `json:"retryable"`
	}{lease.LeaseToken, processErr.Error(), retryable}
	return c.postJobResult(ctx, lease.ID, "fail", body)
}

func (c *Client) postJobResult(ctx context.Context, jobID, operation string, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode worker result: %w", err)
	}
	path := "/worker/v1/jobs/" + url.PathEscape(jobID) + "/" + operation
	request, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("report capture job %s: %w", operation, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return responseError("report capture job "+operation, response)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	endpoint := *c.baseURL
	endpoint.Path += path
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create worker request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	return request, nil
}

func responseError(operation string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("%s: server returned %s: %s", operation, response.Status, message)
}

func decodeJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxResponseBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected data after JSON value")
		}
		return err
	}
	return nil
}
