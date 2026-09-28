// Package dns switches public DNS records through a provider. Cloudflare is
// the first. See docs/design/failover.md, section 6.
package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Zone is a DNS zone the provider's credentials can see.
type Zone struct {
	ID, Name string
}

// Record is a DNS record.
type Record struct {
	ID, Name, Type, Content string
	TTL                     int
	Proxied                 bool
}

// Provider reads and updates DNS records.
type Provider interface {
	Zones(ctx context.Context) ([]Zone, error)
	// Find returns the record with the name and type, or nil.
	Find(ctx context.Context, zone Zone, name, typ string) (*Record, error)
	Update(ctx context.Context, zone Zone, r Record, content string) error
}

// ZoneFor returns the zone a record belongs to: the longest zone name the
// record's name ends with.
func ZoneFor(zones []Zone, name string) (Zone, bool) {
	name = Normalize(name)
	var best Zone
	for _, z := range zones {
		zn := Normalize(z.Name)
		if (name == zn || strings.HasSuffix(name, "."+zn)) && len(zn) > len(best.Name) {
			best = Zone{ID: z.ID, Name: zn}
		}
	}
	return best, best.ID != ""
}

// Normalize makes names and values comparable: lower case (for names),
// without a trailing dot or surrounding quotes.
func Normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".")
	return strings.ToLower(s)
}

// SameValue reports whether two record values are the same, ignoring case,
// a trailing dot, and the quotes TXT values may carry.
func SameValue(a, b string) bool {
	unquote := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			s = s[1 : len(s)-1]
		}
		return Normalize(s)
	}
	return unquote(a) == unquote(b)
}

// Cloudflare is the Cloudflare API with a token that has "Zone: DNS: Edit"
// on the plans' zones.
type Cloudflare struct {
	Token string
	// BaseURL defaults to Cloudflare's API; tests replace it.
	BaseURL string
	Client  *http.Client
}

const cloudflareAPI = "https://api.cloudflare.com/client/v4"

type cfResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

func (c *Cloudflare) do(ctx context.Context, method, path string, body any) (*cfResponse, error) {
	base := c.BaseURL
	if base == "" {
		base = cloudflareAPI
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out cfResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("cloudflare: HTTP %d: unreadable response", resp.StatusCode)
	}
	if !out.Success {
		var msgs []string
		for _, e := range out.Errors {
			msgs = append(msgs, fmt.Sprintf("%s (%d)", e.Message, e.Code))
		}
		if len(msgs) == 0 {
			msgs = append(msgs, "HTTP "+strconv.Itoa(resp.StatusCode))
		}
		return nil, errors.New("cloudflare: " + strings.Join(msgs, "; "))
	}
	return &out, nil
}

// Zones lists the zones the token can see.
func (c *Cloudflare) Zones(ctx context.Context) ([]Zone, error) {
	var zones []Zone
	for page := 1; ; page++ {
		resp, err := c.do(ctx, http.MethodGet, "/zones?per_page=50&page="+strconv.Itoa(page), nil)
		if err != nil {
			return nil, err
		}
		var batch []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(resp.Result, &batch); err != nil {
			return nil, err
		}
		for _, z := range batch {
			zones = append(zones, Zone{ID: z.ID, Name: z.Name})
		}
		if page >= resp.ResultInfo.TotalPages {
			return zones, nil
		}
	}
}

type cfRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// Find returns the record with the name and type, or nil.
func (c *Cloudflare) Find(ctx context.Context, zone Zone, name, typ string) (*Record, error) {
	q := url.Values{"name": {Normalize(name)}, "type": {strings.ToUpper(typ)}}
	resp, err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zone.ID)+"/dns_records?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var recs []cfRecord
	if err := json.Unmarshal(resp.Result, &recs); err != nil {
		return nil, err
	}
	switch len(recs) {
	case 0:
		return nil, nil
	case 1:
		r := recs[0]
		return &Record{ID: r.ID, Name: r.Name, Type: r.Type, Content: r.Content, TTL: r.TTL, Proxied: r.Proxied}, nil
	}
	return nil, fmt.Errorf("%d %s records named %s; EZDR only switches single records", len(recs), typ, name)
}

// Update sets a record's content, keeping its other settings.
func (c *Cloudflare) Update(ctx context.Context, zone Zone, r Record, content string) error {
	_, err := c.do(ctx, http.MethodPatch, "/zones/"+url.PathEscape(zone.ID)+"/dns_records/"+url.PathEscape(r.ID),
		map[string]string{"content": content})
	return err
}
