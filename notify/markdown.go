package notify

import (
	"fmt"
	"strings"
)

// This file holds the shared message rendering for the "Markdown-family" channels (DingTalk, WeCom).
// Feishu uses card JSON, Telegram uses HTML, email uses HTML, each rendered in its own adapter.

// maxAssetsShown is how many assets at most a message lists. One vulnerability may anchor dozens of assets,
// Listing them all would blow up the message with no information value -- nobody reads the 4th and later domains in IM.
const maxAssetsShown = 3

// maxSummaryRunes is how many chars the summary is compressed to. IM messages are "prompts to go see details",
// Not the report body itself; the full content lives in the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is reserved for the message header (summary line + severity distribution + possible truncation notice)
// and the tail (platform links). When packing entry by entry, this part is deducted from the budget so head and tail are never cut off --
// Once the head or tail is cut, readers cannot even tell "which batch this is, or how many items are not shown".
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
//
// Why this is required: vulnerability titles, summaries, types, and asset display names all come from **untrusted sources** --
// Titles and summaries come from model output (the model reads the scanned target's responses), while asset urls come from scanning
// the obtained full URL (including target-controlled query strings). Without escaping, an item titled
//
//	Login-portal SQL injection\n[Urgent: verify your account here](http://attacker.tld)
//
// the vulnerability would render in a security engineer's DingTalk/Feishu as a **clickable external link**; whereas
// `![](http://attacker.tld/beacon)` would be fetched by the client at render time, equivalent to reporting to
// ..."this vulnerability has been viewed" and leak the reader's IP. Even benign content, injected bold or
// A blockquote can also squeeze critical vulnerabilities below the fold line.
//
// The escape set covers titles/links/emphasis/lists/quotes/strikethrough -- constructs that alter structure or produce clickable
// characters of clickable elements. “ must be handled first, otherwise the backslashes added later get escaped a second time.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText compresses untrusted text into one line and escapes it, for use in markdown bodies.
// Single-lining is the other half besides escaping: a newline alone can forge new list items or quote blocks,
// And escape characters can't stop it.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (IM platform title bar/card title); the content is the **unescaped original text**.
//
// Deliberately no escaping here: this title is shared by renderers in four contexts -- markdown body, Telegram's
// HTML, the plain_text of Feishu cards, and the JSON of generic Webhooks and email subjects. Each context's ...
// escaping rules all differ (markdown escaping stuffed into HTML leaves visible backslashes; stuffed into JSON it pollutes
// data), so escaping must be handled by each output endpoint; see writeItem / feishuItemLines /
// telegramEscape. Markdown escaping was once added in a shared function, but as a result, in Telegram messages ...
// visible artifact appeared `\(1\)` such visible backslashes.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Vulnerability digest · %d total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body, returning the body and the **number of entries actually written**.
//
// The return value kept is the number of entries actually delivered in this delivery; the caller uses it to mark only the first kept entries as
// ...marked delivered -- entries blocked by the channel length limit must wait for the next batch, not be ... along with them
// ...marked as successful. This is exactly the source of "silent loss": the message was truncated, but the delivery record shows everything delivered,
// Nothing anywhere shows that the second half was never sent.
//
// maxBytes<=0 means unlimited.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// Even an overlong single message is still sent (final truncation serves as the backstop): partial information about a single vulnerability
		// Still better than sending nothing at all.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all in the platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the opening of the summary message: time window, entry count, and severity distribution.
// With these, recipients of the digest can tell whether this batch needs immediate handling without opening the platform.
//
// items are the entries **actually fitted**, total is the due total of this batch. When they differ it must be stated explicitly
// ..."how many more entries are in the next message" -- otherwise readers will assume the number in the message header is the total,
// Entries that were never sent simply don't exist in the UI.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**%d vulnerabilities added in the last %d minutes**", total, m.WindowMinutes)
	} else {
		fmt.Fprintf(&b, "**%d new vulnerabilities**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "(Showing the first %d entries here; the remaining %d continue in the next message)", len(items), extra)
	}
	// Show the distribution by severity so readers see at a glance whether critical items exist. Only count **entries actually included in this message**
	// Entries, ensuring "severity 3" matches the entries countable below.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders a single vulnerability entry.
//
// prefix is used for the digest list numbering; with single=true renders the full version (with summary and backlink),
// The digest list renders only one line of summary per item -- otherwise a 50-item digest becomes a long document.
//
// All externally sourced content (titles/types/assets/summaries) goes through markdownText:
// single-lining + escaping. The backlink is assembled from the admin-configured public_base_url, not untrusted content,
// And it must be a clickable link, so it is emitted verbatim.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Summary mode: single-line presentation, with compressed assets and summary appended after.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " -- " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s -> %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**type**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Asset**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}
