package mail

import (
	"strings"
	"testing"
)

func TestVisibleHTMLSuppressesHiddenContent(t *testing.T) {
	cases := map[string]string{
		"aria":           `<div aria-hidden="TRUE">hidden text</div>`,
		"offscreen-left": `<div style="left:-9999px; position:absolute">hidden text</div>`,
		"offscreen-top":  `<div style="top:-1000px; position:fixed">hidden text</div>`,
		"indent":         `<div style="text-indent:-100px">hidden text</div>`,
		"clip":           `<div style="clip:rect(1px,1px,1px,1px)">hidden text</div>`,
		"clip-path":      `<div style="clip-path:inset(50%)">hidden text</div>`,
		"clip-circle":    `<div style="clip-path:circle(0 at 50% 50%)">hidden text</div>`,
		"width":          `<div style="width:0; overflow:hidden">hidden text</div>`,
		"height":         `<div style="height:0px; overflow:hidden">hidden text</div>`,
		"max-height":     `<div style="max-height:0">hidden text</div>`,
		"same-color":     `<div style="color:white;background:#fff">hidden text</div>`,
		"inherited-background": `<div style="background-color:#fff">` +
			`<span style="color:rgb(255 255 255)">hidden text</span></div>`,
		"inherited-color": `<div style="color:white"><span style="background:white">hidden text</span></div>`,
		"important":       `<div style="display:none!important;display:block">hidden text</div>`,
		"template":        `<template><div>hidden text</div></template>`,
		"noscript":        `<noscript>hidden text</noscript>`,
		"title":           `<title>hidden text</title>`,
		"metadata":        `<meta content="hidden text"><link href="https://example.com/hidden text">`,
	}
	for name, markup := range cases {
		t.Run(name, func(t *testing.T) {
			preview := visibleHTML([]byte("before" + markup + "after"))
			if strings.Contains(preview, "hidden text") ||
				!strings.Contains(preview, "before") || !strings.Contains(preview, "after") {
				t.Fatalf("unexpected text %q", preview)
			}
		})
	}
}

func TestVisibleHTMLRetainsVisibleNeighbors(t *testing.T) {
	raw := `<div style="background:white">
		<span style="color:black">first</span>
		<span style="color:white">hidden</span>
		<span style="color:red">second</span>
		<span style="left:-9999px;position:relative">third</span>
		<span style="clip-path:inset(5%)">fourth</span>
	</div>`
	if got := strings.Fields(
		visibleHTML([]byte(raw)),
	); strings.Join(
		got,
		" ",
	) != "first second third fourth" {
		t.Fatalf("unexpected visible text %q", got)
	}
}
