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
	root, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return ""
	}

	var text strings.Builder
	appendVisible(&text, root)

	return strings.TrimSpace(text.String())
}

func appendVisible(text *strings.Builder, node *html.Node) {
	if node.Type == html.CommentNode {
		return
	}

	if node.Type == html.TextNode {
		text.WriteString(node.Data)

		return
	}

	if node.Type == html.ElementNode && hidden(node) {
		return
	}

	block := false

	switch node.Data {
	case "br", "p", "div", "section", "article", "blockquote", "li", "tr", "pre",
		"h1", "h2", "h3", "h4", "h5", "h6":
		block = node.Type == html.ElementNode
	}

	if block && text.Len() != 0 {
		text.WriteByte('\n')
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		appendVisible(text, child)
	}

	if block && text.Len() != 0 {
		text.WriteByte('\n')
	}
}

func hidden(node *html.Node) bool {
	switch node.Data {
	case "script", "style", "head", "title", "template", "noscript", "iframe", "svg", "math":
		return true
	}

	for _, attr := range node.Attr {
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
