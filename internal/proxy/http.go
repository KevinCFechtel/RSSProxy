package proxy

import (
	"strconv"
	"strings"
)

func matchesETag(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func stringLength(body []byte) string { return strconv.Itoa(len(body)) }

func safeProcessingError(err error) string {
	// PostgreSQL diagnostics can include source values in constraints/details.
	// Parser and identity errors are useful; database errors are not exposed.
	message := err.Error()
	if strings.Contains(message, "SQLSTATE") || strings.Contains(message, "conn") ||
		strings.Contains(message, "context") || strings.Contains(message, "timeout") {
		return "database operation failed"
	}
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}
