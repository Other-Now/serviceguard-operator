package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeStatusCodes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		healthy bool
	}{
		{"200 ok", 200, true},
		{"204 no content", 204, true},
		{"302 redirect is alive", 302, true},
		{"404", 404, false},
		{"500", 500, false},
		{"503", 503, false},
	}
	p := NewHTTPProber(false)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status == 302 {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			r := p.Probe(context.Background(), srv.URL, time.Second)
			if r.Healthy != tc.healthy || r.StatusCode != tc.status {
				t.Fatalf("got healthy=%v status=%d err=%v, want healthy=%v status=%d", r.Healthy, r.StatusCode, r.Err, tc.healthy, tc.status)
			}
		})
	}
}

func TestProbeTimeoutIsFailure(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	r := NewHTTPProber(false).Probe(context.Background(), srv.URL, 100*time.Millisecond)
	if r.Healthy || r.Err == nil {
		t.Fatalf("slow endpoint should fail, got %+v", r)
	}
	if r.Latency < 100*time.Millisecond || r.Latency > 2*time.Second {
		t.Fatalf("latency %v should be about the timeout", r.Latency)
	}
}

func TestProbeConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if r := NewHTTPProber(false).Probe(context.Background(), url, time.Second); r.Healthy || r.Err == nil {
		t.Fatalf("closed port should fail, got %+v", r)
	}
}

func TestProbeReadsCertExpiry(t *testing.T) {
	notAfter := time.Now().Add(36 * time.Hour).Truncate(time.Second)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t, notAfter)}}
	srv.StartTLS()
	defer srv.Close()

	// Verification on: the self-signed cert is rejected, so no expiry is learned.
	if r := NewHTTPProber(false).Probe(context.Background(), srv.URL, time.Second); r.Healthy || r.CertNotAfter != nil {
		t.Fatalf("verifying prober should reject self-signed cert, got %+v", r)
	}
	// Verification off: healthy, and the expiry is reported.
	r := NewHTTPProber(true).Probe(context.Background(), srv.URL, time.Second)
	if !r.Healthy || r.CertNotAfter == nil || !r.CertNotAfter.Equal(notAfter) {
		t.Fatalf("got %+v, want healthy with NotAfter=%v", r, notAfter)
	}
	if !CertExpiringSoon(r.CertNotAfter, time.Now(), 72*time.Hour) {
		t.Fatal("36h left should be inside a 72h window")
	}
	if CertExpiringSoon(r.CertNotAfter, time.Now(), 24*time.Hour) {
		t.Fatal("36h left should be outside a 24h window")
	}
}

func TestCertExpiringSoonEdges(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	past := now.Add(-time.Hour)
	far := now.Add(1000 * time.Hour)
	cases := []struct {
		name     string
		notAfter *time.Time
		min      time.Duration
		want     bool
	}{
		{"no cert", nil, time.Hour, false},
		{"disabled", &past, 0, false},
		{"already expired", &past, time.Hour, true},
		{"far future", &far, 72 * time.Hour, false},
	}
	for _, tc := range cases {
		if got := CertExpiringSoon(tc.notAfter, now, tc.min); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func selfSigned(t *testing.T, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
