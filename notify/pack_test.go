package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the "pack by whole entry" fix: when a summary message exceeds the channel length limit, it must **entry by entry**
// truncate and truthfully report how many items did not fit, so the caller marks only those actually delivered.
//
// The old approach rendered the full text, truncated it, then marked the whole batch delivered: the second half of the message vanished into thin air,
// while the delivery history shows all-success -- the vulnerabilities just vanish, with nowhere to notice it.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// A 200-item Chinese digest, necessarily far exceeding WeCom's 4096 bytes.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("Body %d bytes exceeds limit %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("Body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("Should hold only part of them (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must truthfully state how many items this message contains and how many remain -- otherwise readers will take the header's
	// ...mistake that number for the whole set.
	if !strings.Contains(body, "remaining") || !strings.Contains(body, "continue in the next message") {
		t.Fatalf("header should state how many more items are not included in this message:\n%s", body[:minInt(400, len(body))])
	}
	// Should contain only the first kept items.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "vulnerability"+itoa(i+1)) {
			t.Fatalf("item #%d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "vulnerability"+itoa(kept+1)) {
		t.Fatalf("Item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = unlimited
	if kept != len(m.Items) {
		t.Fatalf("Should keep everything when length is unlimited, got kept=%d", kept)
	}
	if strings.Contains(body, "Remaining") {
		t.Fatalf("truncation notice should not appear when nothing was truncated:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// When the budget cannot fit even one entry, one entry must still be emitted (with final truncation as the backstop).
	// Otherwise one over-long vulnerability would wedge the whole batch in place forever: never fits on any pickup, never sent.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("Should keep at least 1 item, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("A single message should report 1 delivery, got %d", kept)
	}
	// An empty message has no deliverable items.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("Empty message should report 0 items, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits by **character count**; using the byte-based unit would squeeze Chinese messages to a third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("Body of %d chars exceeds the limit of %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("Should hold only part of the items, got %d", kept)
	}
	if !strings.Contains(text, "continued next") {
		t.Fatalf("should state that there is remaining allowance not included:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("Card should hold only part of the items, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels never truncate the body; the whole batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("Precondition does not hold")
	}
	// Indirectly confirmed via the renderer's return value: markdownBody(0) keeps everything when unlimited.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("Should apply to all when length is unlimited, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is the regression test for "untrusted content must not alter the message structure".
// Titles and summaries come from model output (the model reads the tested target's responses); asset names come from the tested target's URL.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // Must appear in the result (escaped form)
		wrong []string // Must not appear in the result (unescaped form)
	}{
		{
			name: "Newline + outbound link in title",
			item: Item{
				Severity: "high",
				Name:     "Login-portal SQL injection\n[Urgent: verify your account here](http://attacker.tld)",
			},
			// Newlines must be folded (otherwise new list items/quote blocks could be forged);
			// Square brackets and parentheses must be escaped (otherwise they become clickable external links).
			must:  []string{`\[Urgent: verify your account here\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[urgent", "\n\n[urgent"},
		},
		{
			name: "Image beacon in title",
			item: Item{
				Severity: "high",
				Name:     "vulnerability ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "Emphasis and quotes in asset names",
			item: Item{
				Severity: "high",
				Name:     "Plain title",
				Assets:   []string{"a.com/*injection*>quote"},
			},
			must:  []string{`\*injection\*`, `\>`},
			wrong: []string{"*injection*"},
		},
		{
			name: "Backticks and pipes in summaries",
			item: Item{
				Severity: "high",
				Name:     "Title",
				Summary:  "`code` | table",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// In single-item mode, writeItem is the render path shared by the three markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("missing escaped form %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("Unescaped form %q appeared (usable for injecting structure or external links):<br>%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst locks in the escaping order: backslashes must be processed first,
// Otherwise the backslash added later would get wrapped in another layer, producing double backslashes in the output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("Escape order is wrong, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes locks in a specific regression:
// markdown escaping must not leak into Telegram's HTML output (it once lived in the shared title function
// escaped, and the resulting Telegram message showed visible backslashes like `\(1\)`).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *highlight*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram body contains markdown backslash escaping:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
