package mail

import (
	"encoding/hex"
	"math"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/image/colornames"
)

const (
	offscreenPixels = -100
	rectangleFields = 4
	insetAxes       = 2
	rgbHexLength    = 6
	rgbaHexLength   = 8
	rgbChannels     = 3
)

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssEscape  = regexp.MustCompile(`\\(?:[0-9a-fA-F]{1,6}[\t\n\f\r ]?|[^\n\r\f])`)
	cssZero    = regexp.MustCompile(`^[+-]?(?:0+(?:\.0*)?|\.0+)(?:[a-z]+|%)?$`)
	cssPixels  = regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:px)?$`)
	cssRGB     = regexp.MustCompile(`^rgba?\(([0-9.,% /+-]+)\)$`)
)

func declarations(style string) map[string]string {
	values := make(map[string]string)
	important := make(map[string]bool)
	style = cssEscape.ReplaceAllStringFunc(cssComment.ReplaceAllString(style, ""), decodeCSSEscape)
	for declaration := range strings.SplitSeq(strings.ToLower(style), ";") {
		key, value, found := strings.Cut(declaration, ":")
		if !found {
			continue
		}
		value, priority, _ := strings.Cut(value, "!")
		key = strings.TrimSpace(key)
		if key == "background" {
			key = "background-color"
		}
		strong := strings.TrimSpace(priority) == "important"
		if strong || !important[key] {
			values[key] = strings.TrimSpace(value)
			important[key] = strong
		}
	}

	return values
}

func hiddenStyle(values map[string]string) bool {
	for _, property := range []string{"opacity", "font-size", "max-height"} {
		if cssZero.MatchString(values[property]) {
			return true
		}
	}

	return values["display"] == "none" || values["visibility"] == "hidden" ||
		values["visibility"] == "collapse" || hiddenGeometry(values)
}

func hiddenGeometry(values map[string]string) bool {
	absolute := values["position"] == "absolute" || values["position"] == "fixed"
	overflowHidden := values["overflow"] == "hidden" || values["overflow"] == "clip"

	return (overflowHidden && (cssZero.MatchString(values["width"]) || cssZero.MatchString(values["height"]))) ||
		(absolute && (offscreen(values["left"]) || offscreen(values["top"]))) ||
		offscreen(values["text-indent"]) ||
		clipped(values["clip"]) ||
		clipped(values["clip-path"])
}

func offscreen(value string) bool {
	if !cssPixels.MatchString(value) {
		return false
	}
	pixels, err := strconv.ParseFloat(strings.TrimSuffix(value, "px"), 64)

	return err == nil && pixels <= offscreenPixels
}

func clipped(value string) bool {
	name, body, valid := strings.Cut(value, "(")
	if !valid || !strings.HasSuffix(body, ")") {
		return false
	}
	fields := strings.Fields(strings.ReplaceAll(strings.TrimSuffix(body, ")"), ",", " "))
	switch name {
	case "rect", "xywh":
		return emptyRectangle(name, fields)
	case "inset":
		return emptyInset(fields)
	case "circle":
		return len(fields) > 0 && cssZero.MatchString(fields[0])
	case "ellipse":
		return len(fields) >= 2 &&
			(cssZero.MatchString(fields[0]) || cssZero.MatchString(fields[1]))
	case "polygon":
		return len(fields) >= 6 && (flatPolygon(fields, 0) || flatPolygon(fields, 1))
	default:
		return false
	}
}

func emptyRectangle(name string, fields []string) bool {
	if len(fields) != rectangleFields {
		return false
	}
	if name == "xywh" {
		return cssZero.MatchString(fields[2]) || cssZero.MatchString(fields[3])
	}

	return sameLength(fields[0], fields[2]) || sameLength(fields[1], fields[3])
}

func emptyInset(fields []string) bool {
	var offsets []float64
	for _, field := range fields {
		if field == "round" {
			break
		}
		if !strings.HasSuffix(field, "%") {
			return false
		}
		value, err := strconv.ParseFloat(strings.TrimSuffix(field, "%"), 64)
		if err != nil {
			return false
		}
		offsets = append(offsets, value)
	}
	const fullPercent = 100
	switch len(offsets) {
	case 1:
		return offsets[0]*2 >= fullPercent
	case insetAxes:
		return offsets[0]*2 >= fullPercent || offsets[1]*2 >= fullPercent
	case insetAxes + 1:
		return offsets[0]+offsets[2] >= fullPercent || offsets[1]*2 >= fullPercent
	case rectangleFields:
		return offsets[0]+offsets[2] >= fullPercent || offsets[1]+offsets[3] >= fullPercent
	default:
		return false
	}
}

func flatPolygon(fields []string, axis int) bool {
	for i := axis + insetAxes; i < len(fields); i += insetAxes {
		if !sameLength(fields[i], fields[axis]) {
			return false
		}
	}

	return true
}

func sameLength(first, second string) bool {
	if cssZero.MatchString(first) && cssZero.MatchString(second) {
		return true
	}

	return first == second && first != "auto"
}

type inlineColors struct {
	color      string
	background string
}

func (colors inlineColors) apply(values map[string]string) inlineColors {
	if value, ok := values["color"]; ok && value != "inherit" {
		colors.color = literalColor(value)
	}
	background, ok := values["background-color"]

	if ok && background != "transparent" && background != "inherit" {
		colors.background = literalColor(background)
	}

	return colors
}

func literalColor(value string) string {
	if named, ok := colornames.Map[value]; ok {
		return hex.EncodeToString([]byte{named.R, named.G, named.B, named.A})
	}
	if strings.HasPrefix(value, "#") {
		return hexColor(value[1:])
	}
	if match := cssRGB.FindStringSubmatch(value); len(match) != 0 {
		return rgbColor(match[1])
	}

	return ""
}

func hexColor(value string) string {
	if len(value) == 3 || len(value) == 4 {
		var expanded strings.Builder
		for _, char := range value {
			expanded.WriteRune(char)
			expanded.WriteRune(char)
		}
		value = expanded.String()
	}
	if len(value) == rgbHexLength {
		value += "ff"
	}
	if len(value) != rgbaHexLength {
		return ""
	}
	_, err := hex.DecodeString(value)
	if err != nil {
		return ""
	}

	return value
}

func rgbColor(body string) string {
	fields := strings.FieldsFunc(body, func(char rune) bool {
		return char == ' ' || char == ',' || char == '/'
	})
	if len(fields) != rgbChannels && len(fields) != rectangleFields {
		return ""
	}
	const maxChannel = 255
	components := []byte{0, 0, 0, maxChannel}
	for index, field := range fields {
		number, percentage := strings.CutSuffix(field, "%")
		value, err := strconv.ParseFloat(number, 64)
		if err != nil {
			return ""
		}
		switch {
		case percentage:
			const percent = 100
			value *= float64(maxChannel) / percent
		case index == rgbChannels:
			value *= maxChannel
		}
		components[index] = byte(math.Round(max(0, min(maxChannel, value))))
	}

	return hex.EncodeToString(components)
}
