// Package delivery contains the stream output packaging concerns of the Ari
// Media Engine. Phase 1 covers iframe embedding security: a per-stream and
// global domain allow-list enforced through the Content-Security-Policy
// frame-ancestors directive and a request guard middleware.
package delivery

import (
	"net/url"
	"sort"
	"strings"
	"sync"
)

// WildcardAll matches every embedding domain.
const WildcardAll = "*"

// DomainPolicy stores the embedding allow-list. A zero policy (no global
// domains, AllowAll false) denies all cross-origin embedding and permits only
// same-origin framing. It is safe for concurrent use.
type DomainPolicy struct {
	mu        sync.RWMutex
	allowAll  bool
	global    []string
	perStream map[string][]string
}

// NewDomainPolicy builds a policy from an optional global allow-list.
func NewDomainPolicy(global []string, allowAll bool) *DomainPolicy {
	return &DomainPolicy{
		allowAll:  allowAll,
		global:    normalizeDomains(global),
		perStream: make(map[string][]string),
	}
}

// AllowAll reports whether every embedding domain is permitted.
func (p *DomainPolicy) AllowAll() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.allowAll
}

// SetAllowAll toggles the global wildcard.
func (p *DomainPolicy) SetAllowAll(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allowAll = v
}

// Global returns a copy of the global allow-list.
func (p *DomainPolicy) Global() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.global...)
}

// SetGlobal replaces the global allow-list.
func (p *DomainPolicy) SetGlobal(domains []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.global = normalizeDomains(domains)
}

// Stream returns a copy of the per-stream allow-list.
func (p *DomainPolicy) Stream(streamID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.perStream[streamID]...)
}

// SetStream replaces the per-stream allow-list. An empty list clears it so the
// stream falls back to the global policy.
func (p *DomainPolicy) SetStream(streamID string, domains []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(domains) == 0 {
		delete(p.perStream, streamID)
		return
	}
	p.perStream[streamID] = normalizeDomains(domains)
}

// DeleteStream drops any per-stream override.
func (p *DomainPolicy) DeleteStream(streamID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.perStream, streamID)
}

// Effective returns the allow-list that applies to streamID: the per-stream
// override when present, otherwise the global list.
func (p *DomainPolicy) Effective(streamID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if list, ok := p.perStream[streamID]; ok {
		return append([]string(nil), list...)
	}
	return append([]string(nil), p.global...)
}

// IsAllowed reports whether origin may frame streamID. An empty origin (a
// direct top-level navigation, not an embed) is always allowed. When the
// wildcard is enabled every origin is allowed.
func (p *DomainPolicy) IsAllowed(streamID, origin string) bool {
	host := HostFromOrigin(origin)
	if host == "" {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.allowAll {
		return true
	}
	list, ok := p.perStream[streamID]
	if !ok {
		list = p.global
	}
	return hostAllowed(list, host)
}

// FrameAncestors builds the Content-Security-Policy frame-ancestors source list
// for streamID. It always includes 'self' so same-origin embedding keeps
// working, unless the wildcard is enabled, in which case it returns "*".
func (p *DomainPolicy) FrameAncestors(streamID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.allowAll {
		return WildcardAll
	}
	list, ok := p.perStream[streamID]
	if !ok {
		list = p.global
	}
	tokens := []string{"'self'"}
	for _, d := range list {
		if d == WildcardAll {
			return WildcardAll
		}
		tokens = append(tokens, d)
	}
	return strings.Join(tokens, " ")
}

// HostFromOrigin extracts a lowercase host (no port) from an Origin or Referer
// header value. It accepts full URLs and bare host names. It returns "" when no
// usable host is present.
func HostFromOrigin(origin string) string {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return ""
	}
	if strings.Contains(origin, "://") {
		u, err := url.Parse(origin)
		if err != nil {
			return ""
		}
		return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	}
	if strings.HasPrefix(origin, "//") {
		u, err := url.Parse("scheme:" + origin)
		if err != nil {
			return ""
		}
		return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	}
	host := origin
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// normalizeDomains lowercases, trims and de-duplicates configured domains.
func normalizeDomains(domains []string) []string {
	seen := make(map[string]struct{}, len(domains))
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		d = strings.TrimSpace(strings.ToLower(d))
		if d == "" {
			continue
		}
		if d == WildcardAll {
			if _, ok := seen[d]; !ok {
				seen[d] = struct{}{}
				out = append(out, d)
			}
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// hostAllowed reports whether host matches any entry in list. Entries may be a
// bare host, a full origin (scheme://host) or a "*.suffix" wildcard.
func hostAllowed(list []string, host string) bool {
	for _, entry := range list {
		if entry == WildcardAll {
			return true
		}
		if matchDomain(entry, host) {
			return true
		}
	}
	return false
}

// matchDomain matches a single configured entry against a host.
func matchDomain(entry, host string) bool {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if strings.Contains(entry, "://") {
		if u, err := url.Parse(entry); err == nil {
			entry = strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		}
	}
	entry = strings.TrimSuffix(entry, ".")
	switch {
	case entry == "":
		return false
	case entry == host:
		return true
	case strings.HasPrefix(entry, "*."):
		suffix := entry[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	default:
		return false
	}
}
