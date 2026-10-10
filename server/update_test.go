package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache is the layer protecting the GitHub quota: unauthenticated API allows only 60 requests/hour/IP,
// And the top-bar "new version available" hint is checked on every full page load. Once the cache expires, if the user opens several [...]
// Too many tabs would drain the quota, leaving real update checks unable to run later.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 queries should hit the origin only once, actually %d times", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// Clicking "check for updates" must return a real-time result; otherwise a just-released version stays invisible until the cache expires.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force should bypass the cache, expected 2 origin fetches, got %d", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// Move the stored timestamp back to just expired to simulate TTL expiry.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL refetch from origin after TTL expiry, expected 2 time(s), actual %d time(s)", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github unreachable")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("Expected an error return")
	}
	// Failed results should also be cached for a while; otherwise, when GitHub is unreachable, every page load waits out a timeout for nothing.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("Expected an error return")
	}
	if calls != 1 {
		t.Errorf("Errors should be cached briefly; expected 1 origin fetch, got %d", calls)
	}

	// But the error TTL must be significantly shorter than the success one, so it can self-heal quickly after the network recovers.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("Error TTL (%v) must be shorter than success TTL (%v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("Expected an error return")
	}
	if calls != 2 {
		t.Errorf("Should retry after the error TTL expires; expected 2 attempts, got %d", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// A visitor closing the tab cancels the request. That does not mean GitHub has a problem; "canceled" must never be [...]
	// Write it into the cache -- otherwise every visitor in the next 30 minutes gets a baffling error.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // Expire the cache to force a refetch from origin

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("when the caller has cancelled, the error should be passed through to it")
	}

	// Key invariant: the canceled attempt leaves no trace -- the cache holds neither the "canceled" error,
	// Still keeping the last good result.
	if c.err != nil {
		t.Fatalf("cancellation errors must not be cached, got %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("Cache should keep the last good result, got %+v", c.rel)
	}

	// That cancellation fetched no new data, so the next visitor should duly fetch from the origin again -- and get the result normally,
	// Not affected by the previous cancellation.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("Normal requests after cancellation should not error: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("Expected a normal result, got %+v", rel)
	}
}
