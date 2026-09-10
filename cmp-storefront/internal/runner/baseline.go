package runner

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"sort"
	"time"

	"github.com/shopline/cmp-storefront/internal/platform"
	"github.com/shopline/cmp-storefront/internal/stats"
)

// NetBaseline is the network floor under a platform's latency: the cost of
// reaching its edge, independent of any query.
type NetBaseline struct {
	Platform    string  `json:"platform"`
	Host        string  `json:"host"`
	RemoteIP    string  `json:"remote_ip"`
	TCPMedianMS float64 `json:"tcp_connect_median_ms"`
	TCPMinMS    float64 `json:"tcp_connect_min_ms"`
	TLSMedianMS float64 `json:"tls_handshake_median_ms"`
	Samples     int     `json:"samples"`
}

// MeasureBaselines opens fresh connections to each platform's edge and times
// the TCP and TLS handshakes.
//
// Without this a latency comparison is uninterpretable: a platform whose edge
// is one RTT further away looks slower at every percentile no matter how fast
// its servers are. Reporting the floor lets a reader separate "their servers
// are slower" from "their edge is further from this client", and it is the
// difference between a finding and a number.
func MeasureBaselines(ctx context.Context, ps []platform.Platform, samples int) []NetBaseline {
	var out []NetBaseline
	for _, p := range ps {
		if p == nil {
			continue
		}
		u, err := url.Parse(p.Client().Endpoint)
		if err != nil {
			continue
		}
		host := u.Hostname()
		port := u.Port()
		if port == "" {
			if u.Scheme == "http" {
				port = "80"
			} else {
				port = "443"
			}
		}
		b := NetBaseline{Platform: p.Name(), Host: host}
		var tcp, tlsd []float64

		for i := 0; i < samples; i++ {
			if ctx.Err() != nil {
				break
			}
			d := net.Dialer{Timeout: 10 * time.Second}
			t0 := time.Now()
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
			if err != nil {
				continue
			}
			tcp = append(tcp, float64(time.Since(t0))/1e6)
			if a, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
				b.RemoteIP = a.IP.String()
			}
			if u.Scheme == "https" {
				t1 := time.Now()
				tc := tls.Client(conn, &tls.Config{ServerName: host})
				if err := tc.HandshakeContext(ctx); err == nil {
					tlsd = append(tlsd, float64(time.Since(t1))/1e6)
				}
				_ = tc.Close()
			} else {
				_ = conn.Close()
			}
		}

		if len(tcp) > 0 {
			sort.Float64s(tcp)
			b.TCPMedianMS = stats.Quantile(tcp, 0.5)
			b.TCPMinMS = tcp[0]
			b.Samples = len(tcp)
		}
		if len(tlsd) > 0 {
			b.TLSMedianMS = stats.Median(tlsd)
		}
		out = append(out, b)
	}
	return out
}
