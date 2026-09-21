package watch

import (
	"fmt"
	"regexp"
	"strings"
)

var watchPrefix = regexp.MustCompile(`^(?:/[^\s]+/)?watch(?:\s+|$)`)
var watchInterval = regexp.MustCompile(`^(?:-n\s*|--interval(?:=|\s+))([0-9]+(?:\.[0-9]+)?)(?:\s+|$)`)
var watchDisplay = regexp.MustCompile(`^(?:-d|-t|-c|-p|--differences|--no-title|--color|--precise)(?:\s+|$)`)

// NormalizeWatch accepts common watch wrappers, never runs the terminal loop.
// Unknown options and ambiguous outer quoting fail closed; inner shell syntax is preserved.
func NormalizeWatch(input string) (string, error) {
	s := strings.TrimSpace(input)
	prefix := watchPrefix.FindString(s)
	if prefix == "" {
		return s, nil
	}
	s = strings.TrimSpace(s[len(prefix):])
	for strings.HasPrefix(s, "-") {
		if strings.HasPrefix(s, "-- ") {
			s = strings.TrimSpace(s[3:])
			break
		}
		match := watchInterval.FindString(s)
		if match == "" {
			match = watchDisplay.FindString(s)
		}
		if match == "" {
			return "", fmt.Errorf("不支持这个 watch 选项，请填写内部单次命令；采样间隔使用页面设置")
		}
		s = strings.TrimSpace(s[len(match):])
	}
	if len(s) > 0 && (s[0] == '\'' || s[0] == '"') {
		q := s[0]
		if len(s) < 2 || s[len(s)-1] != q || strings.ContainsRune(s[1:len(s)-1], rune(q)) || strings.Contains(s[1:len(s)-1], `\`) {
			return "", fmt.Errorf("watch 外层引号过于复杂，请直接填写单次命令")
		}
		s = s[1 : len(s)-1]
	}
	if strings.TrimSpace(s) == "" || watchPrefix.MatchString(s) {
		return "", fmt.Errorf("请填写 watch 内部的单次命令")
	}
	return s, nil
}
