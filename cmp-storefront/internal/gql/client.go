// Package gql implements a GraphQL-over-HTTP client with per-phase latency
// instrumentation. The phase breakdown is what makes cross-CDN comparison
// meaningful: ServerTime strips DNS/TCP/TLS so two platforms sitting behind
// different edge networks can still be compared on think-time.
package gql

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"
)

// Timing is the per-request phase breakdown. When a pooled connection is
// reused, DNS/Connect/TLS are all zero and TTFB == ServerTime.
type Timing struct {
	DNS       time.Duration `json:"dns_ns"`
	Connect   time.Duration `json:"connect_ns"`
	TLS       time.Duration `json:"tls_ns"`
	TTFB      time.Duration `json:"ttfb_ns"`
	Transfer  time.Duration `json:"transfer_ns"`
	Total     time.Duration `json:"total_ns"`
	Reused    bool          `json:"conn_reused"`
	WireBytes int64         `json:"wire_bytes"`
}

// ServerTime approximates server think-time + origin fetch by removing the
// connection-establishment phases from TTFB. This is the primary metric.
func (t Timing) ServerTime() time.Duration {
	d := t.TTFB - (t.DNS + t.Connect + t.TLS)
	if d < 0 {
		return t.TTFB
	}
	return d
}

// Request is a GraphQL operation to execute.
type Request struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
	Operation string         `json:"operationName,omitempty"`
}

// Response is the raw outcome of one execution, before validation.
type Response struct {
	Timing       Timing
	Status       int
	Body         []byte
	ReqBytes     int
	CacheStatus  string
	RespHeaders  http.Header
	TransportErr error
}

// Errors mirrors the GraphQL error envelope. A 200 with a non-empty errors
// array is a failure: checking the HTTP status alone is the single most common
// way these benchmarks silently compare success against failure.
type GraphQLError struct {
	Message    string         `json:"message"`
	Path       []any          `json:"path"`
	Extensions map[string]any `json:"extensions"`
}

type Errors []GraphQLError

// UnmarshalJSON tolerates the non-standard error envelopes these storefronts
// actually emit. SHOPLINE returns a bare string ({"errors":"Required head
// missing or invalid."}) rather than the spec's array of objects; decoding
// that strictly fails, and the sample would be misfiled as a malformed body
// instead of the auth error it is.
func (e *Errors) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*e = nil
		return nil
	}
	switch trimmed[0] {
	case '[':
		var arr []GraphQLError
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		*e = arr
	case '"':
		var msg string
		if err := json.Unmarshal(trimmed, &msg); err != nil {
			return err
		}
		*e = Errors{{Message: msg}}
	case '{':
		var one GraphQLError
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return err
		}
		*e = Errors{one}
	default:
		*e = Errors{{Message: string(trimmed)}}
	}
	return nil
}

type Envelope struct {
	Data       json.RawMessage `json:"data"`
	Errors     Errors          `json:"errors"`
	Extensions json.RawMessage `json:"extensions"`
}

// Client wraps http.Client with a fixed header set. Each platform gets its own
// Client so connection pools never interfere across the A/B boundary.
// UserAgent identifies the harness explicitly. SHOPLINE denylists some default
// client agents outright -- Python-urllib gets HTTP 436 "Can not access" -- as
// part of its documented "requests must originate from real users" protection.
// Go's default happens to pass today, but relying on a denylist not growing is
// a benchmark that can stop working without warning, and an anonymous default
// is also the wrong thing to point at someone's store.
const UserAgent = "cmpbench/1.0 (storefront-latency-benchmark)"

type Client struct {
	Endpoint string
	Headers  map[string]string
	HTTP     *http.Client
	// Identity forces Accept-Encoding: identity so response bytes are measured
	// uncompressed. Compression ratios track field-name length, so comparing
	// compressed sizes compares naming conventions rather than payload volume.
	Identity bool
}

func NewClient(endpoint string, headers map[string]string, timeout time.Duration, identity bool) *Client {
	tr := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 100,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
		// DisableCompression stops the transport from injecting gzip and
		// transparently decoding it, which would hide the real wire size.
		DisableCompression: identity,
	}
	return &Client{
		Endpoint: endpoint,
		Headers:  headers,
		Identity: identity,
		HTTP:     &http.Client{Transport: tr, Timeout: timeout},
	}
}

// Redact removes this client's credential material from a string. Server
// error messages sometimes reflect request headers back, and those messages end
// up in samples and reports that get pasted into tickets.
func (c *Client) Redact(s string) string {
	for _, v := range c.Headers {
		for _, secret := range []string{v, strings.TrimPrefix(v, "Bearer ")} {
			if len(secret) >= 8 {
				s = strings.ReplaceAll(s, secret, "[REDACTED]")
			}
		}
	}
	return s
}

var cacheHeaders = []string{"cf-cache-status", "x-cache", "x-served-by", "age", "x-sl-cache"}

// Do executes one GraphQL request with full phase instrumentation.
func (c *Client) Do(ctx context.Context, r Request) Response {
	payload, err := json.Marshal(r)
	if err != nil {
		return Response{TransportErr: err}
	}
	out := Response{ReqBytes: len(payload)}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(payload))
	if err != nil {
		out.TransportErr = err
		return out
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	if c.Identity {
		req.Header.Set("Accept-Encoding", "identity")
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}

	var tDNS0, tDNS1, tConn0, tConn1, tTLS0, tTLS1, tFirst time.Time
	var reused bool
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { tDNS0 = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { tDNS1 = time.Now() },
		ConnectStart:         func(string, string) { tConn0 = time.Now() },
		ConnectDone:          func(string, string, error) { tConn1 = time.Now() },
		TLSHandshakeStart:    func() { tTLS0 = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { tTLS1 = time.Now() },
		GotConn:              func(i httptrace.GotConnInfo) { reused = i.Reused },
		GotFirstResponseByte: func() { tFirst = time.Now() },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		out.Timing.Total = time.Since(start)
		out.TransportErr = err
		return out
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body) // Transfer ends only once the body is drained.
	end := time.Now()

	t := Timing{Reused: reused, WireBytes: int64(len(body))}
	if !tDNS0.IsZero() && !tDNS1.IsZero() {
		t.DNS = tDNS1.Sub(tDNS0)
	}
	if !tConn0.IsZero() && !tConn1.IsZero() {
		t.Connect = tConn1.Sub(tConn0)
	}
	if !tTLS0.IsZero() && !tTLS1.IsZero() {
		t.TLS = tTLS1.Sub(tTLS0)
	}
	if !tFirst.IsZero() {
		t.TTFB = tFirst.Sub(start)
		t.Transfer = end.Sub(tFirst)
	}
	t.Total = end.Sub(start)

	out.Timing = t
	out.Status = resp.StatusCode
	out.Body = body
	out.RespHeaders = resp.Header
	out.TransportErr = readErr
	for _, h := range cacheHeaders {
		if v := resp.Header.Get(h); v != "" {
			out.CacheStatus = h + "=" + v
			break
		}
	}
	return out
}

// Parse decodes the GraphQL envelope.
func (r Response) Parse() (*Envelope, error) {
	if len(r.Body) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	var e Envelope
	if err := json.Unmarshal(r.Body, &e); err != nil {
		snippet := string(r.Body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("non-json body (status %d): %s", r.Status, strings.TrimSpace(snippet))
	}
	return &e, nil
}
