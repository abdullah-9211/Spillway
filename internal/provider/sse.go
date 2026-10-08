package provider

import (
	"bufio"
	"io"
	"strings"
)

// SSEReader reads server-sent events from a response body.
type SSEReader struct {
	r *bufio.Reader
}

func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{r: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next event's name and data. Multiple data lines are joined with newlines. It returns
// io.EOF only on a clean end of stream between events; a stream cut inside an event is io.ErrUnexpectedEOF.
func (s *SSEReader) Next() (event, data string, err error) {
	var dataLines []string
	seen := false
	for {
		line, err := s.r.ReadString('\n')
		if err != nil && line == "" {
			if err == io.EOF && seen {
				return "", "", io.ErrUnexpectedEOF
			}
			return "", "", err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if seen {
				return event, strings.Join(dataLines, "\n"), nil
			}
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		case strings.HasPrefix(line, "data:"):
			seen = true
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "event:"):
			seen = true
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if err != nil { // the final line had no newline, so the event was cut off
			if err == io.EOF && seen {
				return "", "", io.ErrUnexpectedEOF
			}
			return "", "", err
		}
	}
}
