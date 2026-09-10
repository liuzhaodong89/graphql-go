package gql

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Canonicalize re-serialises a JSON value with sorted keys and no whitespace.
// Size comparison must run on this form: raw wire bytes vary with the server's
// whitespace and key ordering, which says nothing about how much data was sent.
func Canonicalize(raw json.RawMessage) ([]byte, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var b strings.Builder
	writeCanonical(&b, v)
	return []byte(b.String()), nil
}

func writeCanonical(b *strings.Builder, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			writeCanonical(b, t[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, e)
		}
		b.WriteByte(']')
	default:
		enc, _ := json.Marshal(v)
		b.Write(enc)
	}
}

// FieldBytes flattens a decoded GraphQL `data` value into a leaf-path -> byte
// count map. List indices collapse to "[]" so the two platforms' paths line up
// even when list ordering differs.
//
// This is the diagnostic that makes the 10% size gate actionable: when a
// scenario is over budget it names the field responsible instead of just
// reporting a number.
func FieldBytes(raw json.RawMessage) (map[string]int, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	out := map[string]int{}
	walk("", v, out)
	return out, nil
}

func walk(prefix string, v any, out map[string]int) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			// The key name itself is payload; charge it to the field.
			out[p] += len(k) + 3 // "key": plus separator
			walk(p, sub, out)
		}
	case []any:
		p := prefix + "[]"
		out[p] += 2 + max(0, len(t)-1) // brackets + commas
		for _, e := range t {
			walk(p, e, out)
		}
	default:
		enc, _ := json.Marshal(v)
		out[prefix] += len(enc)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Attribution is one row of the per-field byte diff between two platforms.
type Attribution struct {
	Path       string  `json:"path"`
	BytesA     int     `json:"bytes_a"`
	BytesB     int     `json:"bytes_b"`
	Delta      int     `json:"delta"`
	ShareOfGap float64 `json:"share_of_gap"`
}

// Attribute joins two field-byte maps through a semantic path mapping
// (platform B path -> platform A path) and ranks paths by absolute byte gap.
func Attribute(a, b map[string]int, bToA map[string]string) []Attribution {
	norm := map[string]int{}
	for p, n := range b {
		if mapped, ok := bToA[p]; ok {
			p = mapped
		}
		norm[p] += n
	}
	seen := map[string]bool{}
	var rows []Attribution
	totalGap := 0
	add := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		d := a[p] - norm[p]
		rows = append(rows, Attribution{Path: p, BytesA: a[p], BytesB: norm[p], Delta: d})
		totalGap += abs(d)
	}
	for p := range a {
		add(p)
	}
	for p := range norm {
		add(p)
	}
	for i := range rows {
		if totalGap > 0 {
			rows[i].ShareOfGap = float64(abs(rows[i].Delta)) / float64(totalGap)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return abs(rows[i].Delta) > abs(rows[j].Delta) })
	return rows
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// SizeDelta is the gate metric: |a-b| / max(a,b).
func SizeDelta(a, b int) float64 {
	m := a
	if b > m {
		m = b
	}
	if m == 0 {
		return 0
	}
	return float64(abs(a-b)) / float64(m)
}

func FormatAttribution(rows []Attribution, topN int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-46s %10s %10s %10s %8s\n", "PATH", "A_BYTES", "B_BYTES", "DELTA", "SHARE"))
	for i, r := range rows {
		if i >= topN {
			break
		}
		if r.Delta == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("%-46s %10d %10d %10d %7.1f%%\n",
			trunc(r.Path, 46), r.BytesA, r.BytesB, r.Delta, r.ShareOfGap*100))
	}
	return sb.String()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
