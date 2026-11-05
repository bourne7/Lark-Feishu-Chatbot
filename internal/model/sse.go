package model

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// maxEventBytes is a wire safety ceiling, not a deployment fallback.
const maxEventBytes = 4 << 20

// readSSE handles split network reads, CRLF, comments and multiline data fields.
// [Main Request] 10 Model: bound each SSE event and let the protocol consumer decide completion; premature EOF is
// a failure.
func readSSE(source io.Reader, consume func(string, string) (bool, error)) error {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	var event string
	var data strings.Builder
	hasData := false
	size := 0
	dispatch := func() (bool, error) {
		if !hasData {
			return false, nil
		}
		done, err := consume(event, data.String())
		event, size, hasData = "", 0, false
		data.Reset()
		return done, err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, err := dispatch()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			event = ""
			size = 0
			continue
		}
		size += len(line) + 1
		if size > maxEventBytes {
			return errors.New("model event exceeds safety limit")
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			hasData = true
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.New("model stream read failed")
	}
	if hasData {
		done, err := dispatch()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return errors.New("model stream ended without completion")
}
