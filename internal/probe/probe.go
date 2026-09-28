// Package probe runs one health check against an HTTP(S) endpoint.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Result of one probe. Healthy is false for transport errors, timeouts and
// non-2xx/3xx statuses. CertNotAfter is set whenever a TLS handshake completed,
// even if the HTTP status was bad, so expiry is tracked independently.
type Result struct {
	Healthy      bool
	StatusCode   int
	Latency      time.Duration
	CertNotAfter *time.Time
	Err          error
}

// Prober is the interface the controller depends on, so tests can script results.
type Prober interface {
	Probe(ctx context.Context, url string, timeout time.Duration) Result
}

// HTTPProber is the real implementation.
type HTTPProber struct {
	// InsecureSkipVerify lets the operator read the expiry of self-signed or
	// already-expired certificates. The probe is about liveness and expiry,
	// not trust, so this is deliberate.
	InsecureSkipVerify bool
	client             *http.Client
}

func NewHTTPProber(insecureSkipVerify bool) *HTTPProber {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify}, //nolint:gosec // see field comment
		// A fresh connection per probe, so a dead backend behind a Service is
		// not hidden by a pooled connection to a pod that has since gone away.
		DisableKeepAlives: true,
	}
	return &HTTPProber{
		InsecureSkipVerify: insecureSkipVerify,
		client: &http.Client{
			Transport: tr,
			// Redirects are not followed; a 3xx is itself proof of life.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (p *HTTPProber) Probe(ctx context.Context, url string, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{Err: err}
	}
	start := time.Now()
	resp, err := p.client.Do(req)
	lat := time.Since(start)
	if err != nil {
		return Result{Latency: lat, Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	r := Result{StatusCode: resp.StatusCode, Latency: lat}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		na := resp.TLS.PeerCertificates[0].NotAfter
		r.CertNotAfter = &na
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		r.Healthy = true
	} else {
		r.Err = fmt.Errorf("status %d", resp.StatusCode)
	}
	return r
}

// CertExpiringSoon reports whether notAfter falls inside the warning window.
// minValidity <= 0 disables the check.
func CertExpiringSoon(notAfter *time.Time, now time.Time, minValidity time.Duration) bool {
	if notAfter == nil || minValidity <= 0 {
		return false
	}
	return notAfter.Sub(now) < minValidity
}
