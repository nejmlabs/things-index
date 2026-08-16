package worker

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const clientTestToken = "worker-token-00000000000000000000"

func TestClientTransportSecurity(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		valid   bool
	}{
		{name: "public HTTPS", baseURL: "https://worker.example.com", valid: true},
		{name: "IPv4 loopback HTTP", baseURL: "http://127.0.0.1:8080", valid: true},
		{name: "IPv6 loopback HTTP", baseURL: "http://[::1]:8080", valid: true},
		{name: "public HTTP", baseURL: "http://worker.example.com", valid: false},
		{name: "private HTTP", baseURL: "http://192.168.1.50:8080", valid: false},
		{name: "hostname loopback HTTP", baseURL: "http://localhost:8080", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewClient(ClientConfig{BaseURL: test.baseURL, Token: clientTestToken})
			if test.valid && err != nil {
				t.Fatalf("expected URL to be accepted: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("expected URL to be rejected")
			}
		})
	}
}

func TestDefaultClientDoesNotFollowRedirects(t *testing.T) {
	client, err := NewClient(ClientConfig{BaseURL: "https://worker.example.com", Token: clientTestToken})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://other.example.com/worker/v1/lease", nil)
	if err := client.httpClient.CheckRedirect(request, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy returned %v", err)
	}
}

func TestRetryClassification(t *testing.T) {
	if IsRetryable(&PermanentError{Err: errTestPermanent}) {
		t.Fatal("permanent error was classified as retryable")
	}
	if !IsRetryable(errTestTemporary) {
		t.Fatal("ordinary operational error was classified as permanent")
	}
}

type testError string

func (e testError) Error() string { return string(e) }

const (
	errTestPermanent testError = "permanent"
	errTestTemporary testError = "temporary"
)
