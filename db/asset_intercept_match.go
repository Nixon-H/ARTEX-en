package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// The matching/execution layer of asset intercept rules. asset_intercept.go only handles rule storage; here handles ...
// Matches the domain/IP/URL of "target assets" against enabled rules. For agent tools (add_intent,
// insert_assets) is called before issuing intents / inserting assets; a hit means rejection.

// AssetInterceptKindLabel returns the human-readable kind label for agent-facing messages.
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "Domain (exact match)"
	case "exact_ip":
		return "IP (exact match)"
	case "exact_url":
		return "URL (exact)"
	case "fuzzy_domain":
		return "Domain (fuzzy match)"
	case "fuzzy_ip":
		return "IP (fuzzy match)"
	case "fuzzy_url":
		return "URL (fuzzy)"
	case "cidr":
		return "CIDR range"
	}
	return kind
}

// Reason returns a human-readable hit reason, e.g.: matched asset intercept rule [domain (fuzzy): .gov.cn] (note).
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("Hit asset interception rule [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += " (" + note + ")"
	}
	return s
}

// matchOne checks whether a single enabled rule hits the given domain/IP/URL candidate strings, returning the exact matched value.
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules returns the first enabled rule matching the given domain/IP/URL candidate strings, plus the matched value.
// for insert_assets to match against raw input (not-yet-persisted assetInputItem).
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates extracts the domain/IP/URL candidate strings of a persisted asset for intercept matching.
// The host of a URL is split out and classified, so service-type assets "carrying only a URL" can also be hit by domain/IP rules.
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// InterceptLabel returns a short asset identifier for explanation messages to the agent.
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("Asset #%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule checks whether a rule set contains any enabled rule.
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision is the "block-first-then-allow" gate's verdict on a set of candidate strings.
type AssetGateDecision struct {
	Allowed bool
	Reason  string // Rejection reason (without asset identifier); empty when Allowed=true
}

// EvaluateAssetGate performs the task-level gate decision:
//  1. A hit on any enabled blockRules -> reject (intercept reason).
//  2. Otherwise, if allowRules has enabled entries and none hits -> reject (outside the allow scope).
//
// 3. Otherwise allow.
//
// When allowRules is empty / has no enabled entries, the allow gate is inactive (i.e. no allowlist, everything passes),
// Prevent "no allow rules configured" from blocking all assets.
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "Outside the task's allowed (whitelist) scope; testing is not allowed"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit describes an asset rejected by the gate (intercept hit, or outside the allow scope).
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // Human-readable reason
}

// Describe returns a readable explanation: asset info + reason.
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s -> %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules is a passthrough to the same-named *DB method, letting callers that only hold an AssetStore
// (e.g., agent tools) can also read the rules.
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept loads assets by id and runs the "block-first-then-allow" gate decision on each, returning all
// rejected assets. Block rules = global ∪ task-level block; allow rules = task-level allow (this task only).
// Return early when there is no id. Uses the global GetByIDs (unaffected by task-scope filtering) so interception is not weakened by scope.
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// With neither block rules nor enabled allow rules -> no decision needed, everything passes.
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
