// Package placeholder renders the image served when no thumbnail exists.
//
// It is its own package for a structural reason: the web service must be able
// to answer a thumbnail request without a decoder being linked into it. A
// placeholder is a fixed template — no decode, no queue entry, no disk write —
// so a gallery with thousands of missing thumbnails still renders immediately
// and costs nothing.
package placeholder

import (
	"bytes"
	"fmt"
	"strings"
)

// SVG renders the placeholder for one item.
//
// label is a file name, which is attacker-controlled in the general case, so
// the escaping below is a real defence and not a formality. The output is also
// served as image/svg+xml with nosniff, and the page's CSP forbids inline
// script, so a crafted name cannot reach a scripting context.
func SVG(label string, isVideo bool) []byte {
	safe := escapeXMLText(label, 48)
	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" role="img" aria-label="%s">`, safe)
	b.WriteString(`<rect width="512" height="512" fill="#e9e7e2"/>`)
	b.WriteString(`<g stroke="#b6b1a7" stroke-width="3" fill="none">`)
	if isVideo {
		b.WriteString(`<rect x="120" y="140" width="272" height="200" rx="8"/>` +
			`<path d="M232 196l84 44-84 44z" fill="#b6b1a7" stroke="none"/>`)
	} else {
		b.WriteString(`<rect x="120" y="140" width="272" height="200" rx="8"/>` +
			`<path d="M120 300l84-72 60 52 58-46 70 66"/>`)
	}
	b.WriteString(`</g>`)
	fmt.Fprintf(&b, `<text x="256" y="404" text-anchor="middle" `+
		`font-family="ui-monospace, SFMono-Regular, Menlo, Consolas, monospace" `+
		`font-size="22" fill="#6e6960">%s</text>`, safe)
	b.WriteString(`</svg>`)
	return b.Bytes()
}

// escapeXMLText escapes the five XML special characters and truncates.
func escapeXMLText(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= max {
			b.WriteString("\u2026")
			break
		}
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			if r < 0x20 || r == 0x7f {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
