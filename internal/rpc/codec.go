package rpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// WriteMessage writes one JSON object followed by a newline.
func WriteMessage(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// ReadMessage reads one JSON object (up to the next newline) into v.
func ReadMessage(r *bufio.Reader, v any) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}
