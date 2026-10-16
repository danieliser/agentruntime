package main

import (
	"encoding/json"
	"fmt"
	"io"
)

func WriteOutput(w io.Writer, cfg Config, value any, human string) error {
	if cfg.JSON {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(value)
	}
	if human == "" {
		human = fmt.Sprint(value)
	}
	_, err := fmt.Fprintln(w, human)
	return err
}
