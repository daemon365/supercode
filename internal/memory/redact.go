package memory

import (
	"regexp"
	"strings"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile("(?is)-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----.*?-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
	regexp.MustCompile("(?i)\\bBearer\\s+[A-Za-z0-9._~+/=-]{8,}"),
	regexp.MustCompile("\\bsk-[A-Za-z0-9_-]{12,}\\b"),
	regexp.MustCompile("(?i)(api[_-]?key|access[_-]?token|refresh[_-]?token|authorization|password|secret)(\\s*[=:]\\s*[\"']?)[^\\s,\"'\\]}]{6,}"),
	regexp.MustCompile("(?i)(https?://)[^/@\\s:]+:[^/@\\s]+@"),
}

func redactSecrets(value string) string {
	for index, pattern := range secretPatterns {
		switch index {
		case 1:
			value = pattern.ReplaceAllString(value, "Bearer [REDACTED]")
		case 3:
			value = pattern.ReplaceAllString(value, "$1$2[REDACTED]")
		case 4:
			value = pattern.ReplaceAllString(value, "$1[REDACTED]@")
		default:
			value = pattern.ReplaceAllString(value, "[REDACTED]")
		}
	}
	return strings.TrimSpace(value)
}
