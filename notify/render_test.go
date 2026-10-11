package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is the most critical invariant of this package. WeCom limits by **bytes**, with Chinese text at 3 bytes/character,
	// Any implementation that hard-cuts by bytes splits Chinese characters in half, producing illegal UTF-8 that the platform rejects.
	// Use mixed Chinese-English inputs of coprime lengths to hit every possible cut point.
	inputs := []string{
		"test content for rendering",
		"mixed mixed content content",
		"aMiddleBTextCProbeDTestE",
		"🔴🟠🟡🔵", // 4-byte emoji -- miscuts show up more clearly
		strings.Repeat("vulnerability", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("input %q max=%d: produced invalid UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("input %q max=%d: result of %d bytes exceeds the limit", in, max, len(got))
			}
			// Content must not change when not truncated.
			if len(in) <= max && got != in {
				t.Fatalf("Input %q max=%d: content changed without exceeding the limit -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should mean unlimited")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should mean unlimited")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// When max is smaller than the ellipsis itself, appending the ellipsis must not push the result back over the limit.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1: result %q of length %d exceeds the limit", got, len(got))
	}
	// Normal cases should carry an ellipsis.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("Expected an ellipsis, got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// The caliber difference from TruncateBytes must be kept: Telegram limits length by characters,
	// Using the byte-based unit would cut Chinese messages down to a third.
	s := "one two three four five six seven eight nine ten"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("Expected 5 characters, got %d (%q)", n, got)
	}
	// The same string truncated by bytes should be notably shorter.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("Byte-based truncation should not yield the same char count as rune-based truncation")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("First line\n\nsecond line\twith tab   multiple spaces", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("Should collapse all whitespace, got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("Should not keep consecutive spaces, got %q", got)
	}
	// Must stay readable and valid after truncation.
	got = OneLine("one two three four five six seven eight nine ten", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("Expected 4 characters, got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Truncating HTML directly would cut out fragments like `<a href="htt`, and the platform rejects the whole message.
	s := `<b>Title</b>body text body text body text<a href="https://example.com/very/long/path">View details</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: result of %d characters exceeds the limit", max, n)
		}
		// The tail must not contain an unclosed `<` (i.e. `<` appearing with no `>` in the final segment).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: trailing tag cut off -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("No asset should return an empty string, got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("Should list everything when under the limit, got %q", got)
	}
	// When the limit is exceeded the total must be noted; otherwise readers cannot tell how many assets were left unlisted.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "and 5 in total") {
		t.Fatalf("Should label the total of 5, got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("Empty severity has ordinal 0 and should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("Empty threshold should pass")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("Unknown status mapping, got %q", got)
	}
	// Unknown statuses echo back verbatim; don't invent labels.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("Unknown status should echo back verbatim, got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers a gap flagged by the audit: truncation must avoid more than just
// Half-cut tags, as well as truncated HTML entities to avoid.
//
// After `&amp;` is cut into `&amp`, a parser that only recognizes entities may reject the **entire** message --
// Overlong digest messages are common anyway, so the cost would be too high.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The tail must not contain an entity fragment with "& but no matching ;".
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: trailing entity fragment left: %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: malformed entity appeared", max)
		}
	}
}
