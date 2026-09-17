// Package notices keeps redistribution notices available inside the single EXE.
package notices

import (
	"embed"
	"fmt"
	"io"
)

//go:embed *.txt
var files embed.FS

func WriteTo(w io.Writer) error {
	entries, err := files.ReadDir(".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		data, err := files.ReadFile(entry.Name())
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "\n--- %s ---\n%s\n", entry.Name(), data); err != nil {
			return err
		}
	}
	return nil
}
