package bridge

import (
	"regexp"
	"strings"
)

var (
	mdFence      = regexp.MustCompile("(?m)^\\s*```[^\\n]*$")
	mdHeading    = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s+`)
	mdQuote      = regexp.MustCompile(`(?m)^\s{0,3}>\s?`)
	mdHR         = regexp.MustCompile(`(?m)^\s{0,3}([-*_])(\s*[-*_]){2,}\s*$`)
	mdImage      = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]*)[^)]*\)`)
	mdLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)[^)]*\)`)
	mdBoldStar   = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdBoldUnder  = regexp.MustCompile(`__([^_\n]+)__`)
	mdItalicStar = regexp.MustCompile(`(^|[^*\w])\*([^*\s][^*\n]*)\*`)
	mdStrike     = regexp.MustCompile(`~~([^~\n]+)~~`)
	mdCode       = regexp.MustCompile("`([^`\\n]+)`")
	mdBullet     = regexp.MustCompile(`(?m)^(\s*)[*+]\s+`)
	mdBlankRuns  = regexp.MustCompile(`\n{3,}`)
)

// StripMarkdown converts markdown to readable plain text before it is sent
// through WeChat customer service, which has no markdown rendering
// (docs/03 §5: text/markdown 作为普通文本适配). Link targets are kept in
// parentheses so no information is silently dropped. It is not a full
// CommonMark parser: unknown syntax is left verbatim, never removed.
func StripMarkdown(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = mdFence.ReplaceAllString(s, "")
	s = mdHR.ReplaceAllString(s, "")
	s = mdHeading.ReplaceAllString(s, "")
	s = mdQuote.ReplaceAllString(s, "")
	s = mdImage.ReplaceAllString(s, "$1 ($2)")
	s = mdLink.ReplaceAllStringFunc(s, func(m string) string {
		p := mdLink.FindStringSubmatch(m)
		if p[1] == p[2] {
			return p[1]
		}
		return p[1] + " (" + p[2] + ")"
	})
	s = mdBoldStar.ReplaceAllString(s, "$1")
	s = mdBoldUnder.ReplaceAllString(s, "$1")
	s = mdItalicStar.ReplaceAllString(s, "$1$2")
	s = mdStrike.ReplaceAllString(s, "$1")
	s = mdCode.ReplaceAllString(s, "$1")
	s = mdBullet.ReplaceAllString(s, "$1- ")
	s = mdBlankRuns.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
