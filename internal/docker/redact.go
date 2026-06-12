package docker

import (
	"bytes"
	"io"
	"regexp"
)

var (
	dockerSecretPairRE   = regexp.MustCompile(`(?i)\b([A-Z0-9_.-]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API[_-]?KEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY|AUTHORIZATION|COOKIE|JWT|SESSION)[A-Z0-9_.-]*)(\s*[:=]\s*)([^\s,;&]+)`)
	dockerBearerRE       = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	dockerJWTRE          = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
	dockerURLUserInfoRE  = regexp.MustCompile(`(?i)\b(https?://)[^\s/@:]+:[^\s/@]+@`)
	dockerInlineSecretRE = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key)\b\s+is\s+[^\s,;&]+`)
)

func RedactLogText(value string) string {
	value = dockerURLUserInfoRE.ReplaceAllString(value, `${1}[REDACTED]@`)
	value = dockerBearerRE.ReplaceAllString(value, `Bearer [REDACTED]`)
	value = dockerJWTRE.ReplaceAllString(value, `[REDACTED_JWT]`)
	value = dockerSecretPairRE.ReplaceAllString(value, `$1$2[REDACTED]`)
	value = dockerInlineSecretRE.ReplaceAllString(value, `$1 is [REDACTED]`)
	return value
}

type redactingWriter struct {
	writer  io.Writer
	pending []byte
}

func newRedactingWriter(writer io.Writer) *redactingWriter {
	if writer == nil {
		return nil
	}
	return &redactingWriter{writer: writer}
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	if w == nil || w.writer == nil {
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	for {
		index := bytes.IndexByte(w.pending, '\n')
		if index < 0 {
			break
		}
		line := string(w.pending[:index+1])
		if _, err := w.writer.Write([]byte(RedactLogText(line))); err != nil {
			return 0, err
		}
		w.pending = append([]byte(nil), w.pending[index+1:]...)
	}
	return len(p), nil
}

func (w *redactingWriter) Flush() error {
	if w == nil || w.writer == nil || len(w.pending) == 0 {
		return nil
	}
	_, err := w.writer.Write([]byte(RedactLogText(string(w.pending))))
	w.pending = nil
	return err
}
