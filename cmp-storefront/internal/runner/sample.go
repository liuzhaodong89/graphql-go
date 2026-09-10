package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopline/cmp-storefront/internal/gql"
	"github.com/shopline/cmp-storefront/internal/platform"
	"github.com/shopline/cmp-storefront/internal/scenario"
)

// FailKind classifies why a sample was rejected. The distribution of these
// matters as much as the latency numbers: non-random rejection creates
// survivorship bias, and a platform that fails precisely on its slow requests
// would otherwise look faster than it is.
type FailKind string

const (
	FailNone      FailKind = ""
	FailTransport FailKind = "transport"
	FailHTTP      FailKind = "http_status"
	FailNotJSON   FailKind = "not_json"
	FailGraphQL   FailKind = "graphql_errors"
	FailNullData  FailKind = "null_data"
	FailMissing   FailKind = "missing_required_path"
	FailEcho      FailKind = "echo_mismatch"
	FailThrottled FailKind = "throttled"
)

type Sample struct {
	Platform    string         `json:"platform"`
	Scenario    string         `json:"scenario"`
	Key         string         `json:"key"`
	At          time.Time      `json:"at"`
	OK          bool           `json:"ok"`
	FailKind    FailKind       `json:"fail_kind,omitempty"`
	FailMsg     string         `json:"fail_msg,omitempty"`
	Timing      gql.Timing     `json:"timing"`
	ReqBytes    int            `json:"req_bytes"`
	WireBytes   int64          `json:"wire_bytes"`
	DataBytes   int            `json:"data_bytes"`
	CacheStatus string         `json:"cache_status,omitempty"`
	Fields      map[string]int `json:"-"`
	// RawData is retained only when field collection is requested; keeping
	// every body would dominate memory on a long run.
	RawData json.RawMessage `json:"-"`
}

func (s Sample) ServerMS() float64 { return float64(s.Timing.ServerTime()) / 1e6 }
func (s Sample) TotalMS() float64  { return float64(s.Timing.Total) / 1e6 }

// Exec runs one scenario against one platform and applies the full status gate.
// The return value is named so the redaction defer applies to what the caller
// actually receives.
func Exec(ctx context.Context, p platform.Platform, sc scenario.Scenario, v platform.Vars, key string, collectFields bool) (s Sample) {
	s = Sample{Platform: p.Name(), Scenario: sc.ID, Key: key, At: time.Now()}
	// Credentials must not survive into a sample: failure messages are
	// serialised, aggregated into discard reasons, and printed in reports.
	defer func() { s.FailMsg = redact(p, v, s.FailMsg) }()

	b, err := p.Bind(sc, v)
	if err != nil {
		s.FailKind, s.FailMsg = FailTransport, err.Error()
		return s
	}

	resp := p.Client().Do(ctx, gql.Request{
		Query: b.Query, Variables: b.Variables, Operation: b.Operation,
	})
	s.Timing = resp.Timing
	s.ReqBytes = resp.ReqBytes
	s.WireBytes = resp.Timing.WireBytes
	s.CacheStatus = resp.CacheStatus

	if resp.TransportErr != nil {
		s.FailKind, s.FailMsg = FailTransport, resp.TransportErr.Error()
		return s
	}
	if resp.Status == 429 {
		s.FailKind, s.FailMsg = FailThrottled, "HTTP 429"
		return s
	}
	if resp.Status != 200 {
		s.FailKind, s.FailMsg = FailHTTP, fmt.Sprintf("HTTP %d", resp.Status)
		return s
	}

	env, err := resp.Parse()
	if err != nil {
		s.FailKind, s.FailMsg = FailNotJSON, err.Error()
		return s
	}
	// A GraphQL 200 carrying an errors array is a failed request. Treating it
	// as success is the most common way these comparisons end up timing an
	// error path on one side against a real response on the other.
	if len(env.Errors) > 0 {
		msg := env.Errors[0].Message
		kind := FailGraphQL
		if isThrottle(env.Errors) {
			kind = FailThrottled
		}
		s.FailKind, s.FailMsg = kind, msg
		return s
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		s.FailKind, s.FailMsg = FailNullData, "data is null"
		return s
	}

	var decoded any
	dec := json.NewDecoder(strings.NewReader(string(env.Data)))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		s.FailKind, s.FailMsg = FailNotJSON, err.Error()
		return s
	}

	for _, canonical := range sc.RequiredPaths {
		actual := b.Resolve(canonical)
		val, ok := lookup(decoded, actual)
		if !ok || val == nil {
			s.FailKind = FailMissing
			s.FailMsg = canonical
			if actual != canonical {
				s.FailMsg += " (resolved: " + actual + ")"
			}
			return s
		}
		if arr, isArr := val.([]any); isArr && len(arr) == 0 {
			s.FailKind = FailMissing
			s.FailMsg = fmt.Sprintf("%s is an empty list", canonical)
			return s
		}
	}

	if sc.EchoCheck[0] != "" {
		want, _ := b.Variables[sc.EchoCheck[1]].(string)
		got, ok := lookup(decoded, b.Resolve(sc.EchoCheck[0]))
		if want != "" && (!ok || fmt.Sprint(got) != want) {
			s.FailKind = FailEcho
			s.FailMsg = fmt.Sprintf("%s = %v, want %q", sc.EchoCheck[0], got, want)
			return s
		}
	}

	canon, err := gql.Canonicalize(env.Data)
	if err != nil {
		s.FailKind, s.FailMsg = FailNotJSON, err.Error()
		return s
	}
	s.DataBytes = len(canon)

	if collectFields {
		s.RawData = env.Data
		raw, err := gql.FieldBytes(env.Data)
		if err == nil {
			s.Fields = make(map[string]int, len(raw))
			for k, n := range raw {
				s.Fields[b.RewritePath(k)] += n
			}
		}
	}

	s.OK = true
	return s
}

// redact strips this platform's token and the login fixture's password from a
// message before it is stored anywhere.
func redact(p platform.Platform, v platform.Vars, msg string) string {
	if msg == "" {
		return msg
	}
	msg = p.Client().Redact(msg)
	if len(v.Password) >= 4 {
		msg = strings.ReplaceAll(msg, v.Password, "[REDACTED]")
	}
	return msg
}

// throttleCodes covers both platforms' vocabularies. Shopify says THROTTLED;
// SHOPLINE's documented rate-limit code is REQUEST_FREQUENTLY. Missing one
// means those samples are filed as generic GraphQL errors and the throttler
// never backs off, so the run keeps hammering a limit it cannot see.
var throttleCodes = []string{"THROTTLE", "RATE_LIMIT", "REQUEST_FREQUENTLY", "TOO_MANY_REQUESTS"}

var throttleMessages = []string{"throttle", "rate limit", "too many requests", "request frequently"}

func isThrottle(errs gql.Errors) bool {
	for _, e := range errs {
		if c, ok := e.Extensions["code"].(string); ok {
			u := strings.ToUpper(c)
			for _, code := range throttleCodes {
				if strings.Contains(u, code) {
					return true
				}
			}
		}
		if c, ok := e.Extensions["i18nCode"].(string); ok {
			u := strings.ToUpper(c)
			for _, code := range throttleCodes {
				if strings.Contains(u, code) {
					return true
				}
			}
		}
		lower := strings.ToLower(e.Message)
		for _, m := range throttleMessages {
			if strings.Contains(lower, m) {
				return true
			}
		}
	}
	return false
}

// lookup walks a dotted path through decoded JSON. A path segment applied to a
// list is resolved against the first element, which is enough for presence
// checks.
func lookup(v any, path string) (any, bool) {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch t := cur.(type) {
		case map[string]any:
			next, ok := t[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			if len(t) == 0 {
				return nil, false
			}
			m, ok := t[0].(map[string]any)
			if !ok {
				return nil, false
			}
			next, ok := m[seg]
			if !ok {
				return nil, false
			}
			cur = next
		default:
			return nil, false
		}
	}
	return cur, true
}
