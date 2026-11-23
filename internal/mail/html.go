package mail

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

type visibleElement struct {
	name   string
	colors inlineColors
}

func visibleHTML(raw []byte) string {
	tokens := html.NewTokenizer(bytes.NewReader(raw))
	var text strings.Builder
	var ancestors []visibleElement
	for kind := tokens.Next(); kind != html.ErrorToken; kind = tokens.Next() {
		switch kind { //nolint:exhaustive // Only text and element tokens can contribute visible content.
		case html.StartTagToken, html.SelfClosingTagToken:
			tag := tokens.Token()
			element, omit := inspectElement(tag, ancestors)
			if omit {
				if !voidElement(tag.Data) {
					skipElement(tokens, tag.Data)
				}

				continue
			}
			if !voidElement(tag.Data) {
				ancestors = append(ancestors, element)
			}
			blockBreak(&text, tag.Data)
		case html.EndTagToken:
			name, _ := tokens.TagName()
			ancestors = closeVisible(ancestors, string(name))
			blockBreak(&text, string(name))
		case html.TextToken:
			text.Write(tokens.Text())
		default:
			// Comments and declarations have no visible text.
		}
	}

	return strings.TrimSpace(text.String())
}

func skipElement(tokens *html.Tokenizer, element string) {
	depth := 1
	for depth > 0 {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			return
		}
		name, _ := tokens.TagName()
		if string(name) != element {
			continue
		}
		if kind == html.EndTagToken {
			depth--
		} else {
			depth++
		}
	}
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

func inspectElement(tag html.Token, ancestors []visibleElement) (visibleElement, bool) {
	values := elementStyle(tag)
	var colors inlineColors
	if len(ancestors) > 0 {
		colors = ancestors[len(ancestors)-1].colors
	}
	colors = colors.apply(values)
	omit := hidden(tag) || hiddenStyle(values) ||
		(colors.color != "" && colors.color == colors.background)

	return visibleElement{name: tag.Data, colors: colors}, omit
}

func closeVisible(ancestors []visibleElement, name string) []visibleElement {
	for i, element := range slices.Backward(ancestors) {
		if element.name == name {
			return ancestors[:i]
		}
	}

	return ancestors
}

func elementStyle(tag html.Token) map[string]string {
	for _, attr := range tag.Attr {
		if attr.Key == "style" {
			return declarations(attr.Val)
		}
	}

	return nil
}

func hidden(tag html.Token) bool {
	switch tag.Data {
	case "script",
		"style",
		"head",
		"title",
		"template",
		"noscript",
		"iframe",
		"svg",
		"math",
		"meta",
		"link":
		return true
	}
	for _, attr := range tag.Attr {
		if attr.Key == "hidden" ||
			(attr.Key == "aria-hidden" && strings.EqualFold(strings.TrimSpace(attr.Val), "true")) {
			return true
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
