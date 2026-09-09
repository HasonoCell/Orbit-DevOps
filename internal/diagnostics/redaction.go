package diagnostics

import "regexp"

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
	for _, pattern := range commonCredentialPatterns {
		value = pattern.expression.ReplaceAllString(value, pattern.replacement)
	}
	return limitRunes(value, maximumRunes)
}

func limitRunes(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}
