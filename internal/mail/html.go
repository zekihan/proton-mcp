package mail

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssEscape  = regexp.MustCompile(`\\(?:[0-9a-fA-F]{1,6}[\t\n\f\r ]?|[^\n\r\f])`)
	cssZero    = regexp.MustCompile(`^[+-]?(?:0+(?:\.0*)?|\.0+)(?:[a-z]+|%)?$`)
)

func visibleHTML(raw []byte) string {
	tokens := html.NewTokenizer(bytes.NewReader(raw))
	var text strings.Builder
	var suppressed string
	depth := 0
	for kind := tokens.Next(); kind != html.ErrorToken; kind = tokens.Next() {
		switch kind { //nolint:exhaustive // Only text and element tokens can contribute visible content.
		case html.StartTagToken, html.SelfClosingTagToken:
			tag := tokens.Token()
			switch {
			case suppressed != "":
				if tag.Data == suppressed {
					depth++
				}
			case hidden(tag):
				if !voidElement(tag.Data) {
					suppressed, depth = tag.Data, 1
				}
			default:
				blockBreak(&text, tag.Data)
			}
		case html.EndTagToken:
			name, _ := tokens.TagName()
			switch {
			case suppressed == "":
				blockBreak(&text, string(name))
			case string(name) == suppressed:
				depth--
				if depth == 0 {
					suppressed = ""
				}
			}
		case html.TextToken:
			if suppressed == "" {
				text.Write(tokens.Text())
			}
		default:
			// Comments and declarations have no visible text.
		}
	}

	return strings.TrimSpace(text.String())
}

func blockBreak(text *strings.Builder, name string) {
	switch name {
	case "br", "p", "div", "section", "article", "blockquote", "li", "tr", "pre",
		"h1", "h2", "h3", "h4", "h5", "h6":
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
	}
}

func voidElement(name string) bool {
	switch name {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta",
		"param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

func hidden(tag html.Token) bool {
	switch tag.Data {
	case "script", "style", "head", "title", "template", "noscript", "iframe", "svg", "math":
		return true
	}
	for _, attr := range tag.Attr {
		if attr.Key == "hidden" || (attr.Key == "style" && hiddenStyle(attr.Val)) {
			return true
		}
	}

	return false
}

func hiddenStyle(style string) bool {
	style = cssEscape.ReplaceAllStringFunc(cssComment.ReplaceAllString(style, ""), decodeCSSEscape)
	for declaration := range strings.SplitSeq(strings.ToLower(style), ";") {
		key, value, _ := strings.Cut(declaration, ":")
		value, _, _ = strings.Cut(value, "!")

		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "display":
			if value == "none" {
				return true
			}
		case "visibility":
			if value == "hidden" || value == "collapse" {
				return true
			}
		case "font-size", "opacity":
			if cssZero.MatchString(value) {
				return true
			}
		}
	}

	return false
}

func decodeCSSEscape(escape string) string {
	value := strings.TrimRightFunc(escape[1:], unicode.IsSpace)

	code, err := strconv.ParseUint(value, 16, 32)
	if err != nil {
		return escape[1:]
	}

	if code == 0 || code > utf8.MaxRune {
		return "\uFFFD"
	}

	return string(rune(code))
}
