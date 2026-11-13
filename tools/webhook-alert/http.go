package webhookalert

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
)

// DefaultTimeout is how long the webhook has to answer.
const DefaultTimeout = 10 * time.Second

// HTTPSender posts alerts to the owner's webhook as JSON.
//
// The address is treated as a credential: it is never logged and never put
// in an error. Redirects are not followed, so an alert cannot be sent on to
// an address nobody configured.
type HTTPSender struct {
	address string
	client  *http.Client
}

// NewHTTPSender checks the address and returns a sender for it. A timeout of
// zero means DefaultTimeout.
func NewHTTPSender(address string, timeout time.Duration) (*HTTPSender, error) {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, codes.Broken("webhook address is not usable")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &HTTPSender{
		address: address,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Send posts one alert.
//
// Any 2xx answer means delivered. No answer, or an answer of 408, 429 or
// 5xx, means the endpoint is not ready and the alert should be tried again:
// E_TIMEOUT. Any other answer, a redirect included, is unexpected: E_PARSE.
func (s *HTTPSender) Send(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.address, bytes.NewReader(body))
	if err != nil {
		return codes.Broken("webhook address is not usable")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bellbuoy")
	resp, err := s.client.Do(req)
	if err != nil {
		// The text of this error holds the address. Drop it.
		return codes.TimedOut("webhook did not answer in time")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return nil
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		return codes.TimedOut("webhook is not ready")
	default:
		return codes.Broken("webhook gave an unexpected answer")
	}
}
