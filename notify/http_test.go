package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// This file covers the HTTP-layer error classification of doJSON.
//
// Why test this separately: each channel adapter only handles its own platform's business error codes (DingTalk errcode,
// ... the Feishu code and Telegram ok fields), while the **HTTP-layer** classification is done uniformly by doJSON, ...
// The two are independent lines of defense. Without this one, a relay gateway returning 503 would be treated as a permanent failure,
// Retry is abandoned outright; a 403, by contrast, would be treated as retryable and burn three backoff rounds for nothing.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 success is not an error", 200, false},
		{"429 rate-limit is retryable", 429, false},
		{"408 request timeout is retryable", 408, false},
		{"500 server error is retryable", 500, false},
		{"502 gateway error is retryable", 502, false},
		{"503 service unavailable is retryable", 503, false},
		{"400 bad params is permanent failure", 400, true},
		{"401 auth failure is permanent failure", 401, true},
		{"403 forbidden is permanent failure", 403, true},
		{"404 address-not-found is permanent failure", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx should not be an error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Non-2xx should be an error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d permanent determination error: expected %v got %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// The status code must appear in the error; otherwise the user cannot tell whether they misconfigured something or the peer is down.
			// Assert the digits rather than Go's English StatusText: this package's copy is in Chinese
			// (Consistent with the rest of the project): numbers are the language-independent, stably assertable part.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("the error message should carry the HTTP status code %d, got %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet covers snippet: the error description returned by the peer must be brought back,
// Otherwise users only know it "failed" without knowing why the peer rejected it.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("Should error")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("Error message should carry the peer's explanation, got %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded constrains the shape of snippet:
// Its peer response goes verbatim into the last_error column and frontend table; multi-line/overlong content breaks layout and payload.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// A response with newlines, tab characters, and 5000-character oversized content.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("Should error")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("Error message should be flattened to one line, got %q", msg)
	}
	// snippet is capped at 200 characters + a fixed prefix; the total must stay far smaller than the original response.
	if len(msg) > 400 {
		t.Errorf("Error message too long (%d bytes), should be truncated by snippet: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse confirms reads are bounded: when the peer abnormally returns oversized content ...
// Must not read the entire response into memory (every delivery-history entry stores its own copy of last_error).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("Should error")
	}
	if len(err.Error()) > 400 {
		t.Errorf("Oversized responses should be length-limited and truncated, error message length %d", len(err.Error()))
	}
}
