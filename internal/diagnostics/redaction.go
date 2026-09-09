package diagnostics

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var commonCredentialPatterns = []struct {
	expression  *regexp.Regexp
	replacement string
}{
	{
		expression:  regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)([^\s,;]+)`),
		replacement: `${1}[REDACTED]`,
	},
	{
		expression: regexp.MustCompile(
			`(?i)((?:password|passwd|pwd|token|api[_-]?key|secret|access[_-]?key)\s*[=:]\s*)([^\s,;]+)`,
		),
		replacement: `${1}[REDACTED]`,
	},
}

// SanitizeEvidenceText 先约束待处理输入，再遮蔽常见凭据模式，并再次保证 Unicode 长度上限。
func SanitizeEvidenceText(value string, maximumRunes int) string {
	if maximumRunes <= 0 {
		return ""
	}
	value = limitRunes(value, maximumRunes)
	value = redactCommonCredentials(value)
	return limitRunes(value, maximumRunes)
}

// SanitizeRuntimeLog 将任意输入修复为 UTF-8、遮蔽常见凭据，并限制最终响应字节数。
func SanitizeRuntimeLog(value string, maximumBytes int) (string, bool) {
	if maximumBytes <= 0 {
		return "", value != ""
	}
	truncatedBeforeRedaction := len(value) > maximumBytes
	if truncatedBeforeRedaction {
		value = value[:maximumBytes]
	}
	value = strings.ToValidUTF8(value, "�")
	value = redactCommonCredentials(value)
	value, truncatedAfterRedaction := limitUTF8Bytes(value, maximumBytes)
	return value, truncatedBeforeRedaction || truncatedAfterRedaction
}

func redactCommonCredentials(value string) string {
	for _, pattern := range commonCredentialPatterns {
		value = pattern.expression.ReplaceAllString(value, pattern.replacement)
	}
	return value
}

func limitUTF8Bytes(value string, maximum int) (string, bool) {
	if len(value) <= maximum {
		return value, false
	}
	end := maximum
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end], true
}

func limitRunes(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}
