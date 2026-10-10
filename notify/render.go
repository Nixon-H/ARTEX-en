package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…" // single-char ellipsis: rune budgets assume len(ellipsis)==1

// TruncateBytes truncates s to at most max bytes, ensuring the result is valid UTF-8 without splitting a character.
//
// Why cutting must follow character boundaries: WeCom group-bot markdown has a 4096-**byte** hard limit (not
// character count), while one Chinese character takes 3 bytes. Slicing directly by bytes would split a character
// in half, producing invalid
// UTF-8 -- the platform side either rejects the whole message or shows mojibake blocks. The approach here is to first ... from the budget position
// Step back to the nearest rune-start byte (utf8.RuneStart identifies continuation bytes 0b10xxxxxx).
//
// max<=0 means unlimited. An ellipsis is appended after truncation, unless max is too small to even hold the ellipsis.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// When max is shorter than the ellipsis: drop the ellipsis and truncate purely, so the result does not end up exceeding max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine presses multi-line text into one line: collapse all whitespace, then truncate by character count.
// Title line for IM messages -- summaries often contain newlines, which would break the layout if stuffed directly into tables/titles.
// max<=0 means unlimited length.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), appending an ellipsis on overflow.
// max<=0 means unlimited.
//
// The difference from TruncateBytes is the platform convention: WeCom limits by bytes, Telegram by character count.
// Using the wrong unit never errors, it just truncates the message far shorter than expected (1 Chinese character = 3 bytes,
// cutting 4096 by bytes leaves only ~1365 characters), so both functions must be kept and selected per channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count while guaranteeing no half-cut tags.
//
// Truncating HTML directly by characters produces broken tags like `<a href="htt`; the platform parser either
// ...reject the whole message with an error, or swallow the following body as an attribute value. The approach here: first truncate by characters,
// Then check whether the tail has an unclosed `<`; if so, retreat to before it.
//
// No tag balancing (completing </b> and the like): Telegram's HTML parser auto-closes unclosed tags,
// ...while implementing balancing ourselves would require handling quotes in attributes, comments, and self-closing tags -- complexity disproportionate to the benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with `<` (no `>` after the last `<`), roll back to before the `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a cut-off HTML entity (e.g. `&amp;` cut into `&amp`), it must likewise step back.
	// A fragmentary entity may cause **the entire message** to be rejected by an entity-only parser -- one overlong
	// Digest messages hitting the length limit are common in the first place; not worth dropping the whole notification over it.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount computes how many items fit **completely** within budget, for packing digest messages whole-item by whole-item.
//
// Why pack whole items instead of rendering the full piece and then truncating: truncation makes the second-half items vanish into thin air,
// while their delivery records are still marked as delivered -- invisible in the message and invisible in delivery history,
// The vulnerabilities just vanish. After packing whole items, the items that do not fit stay in the store as the next batch,
// The kept value the caller receives is the number of items in this message actually delivered.
//
// Params: maxSize<=0 means no limit; reserve is the amount reserved for message head/tail;
// size handles measuring (each platform differs: WeCom/DingTalk by bytes, Telegram by char count --
// Using the wrong unit never errors, it just squeezes Chinese messages far below the limit);
// render renders the idx-th entry into its actual text -- length varies with content and cannot be estimated.
//
// Return at least 1 (as long as entries remain). Even an extremely overlong single entry must be emitted, with the caller's
// Final truncation fallback; otherwise one oversized vulnerability would permanently stall the whole batch in place.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are the two measurement units of packItemCount, named explicitly to spare call sites ...
// A bare func(s string) int closure appears, otherwise it's hard to tell at a glance which rule is used.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders the asset list as one line of display text, omitting the rest and noting the total when exceeding limit.
// One vulnerability may anchor dozens of assets; listing them all would burst the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " and " + itoa(len(assets)) + " in total"
}

// itoa is a short alias for strconv.Itoa, used only for assembling display text, so strconv need not be imported everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
