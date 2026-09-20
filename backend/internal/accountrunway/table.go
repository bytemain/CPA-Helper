package accountrunway

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// tableWriter is a thin text/tabwriter helper for aligned terminal output.
type tableWriter struct {
	w *tabwriter.Writer
}

func newTableWriter(out io.Writer) *tableWriter {
	return &tableWriter{w: tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)}
}

func (t *tableWriter) row(cells ...string) {
	fmt.Fprintln(t.w, strings.Join(cells, "\t"))
}

func (t *tableWriter) flush() {
	_ = t.w.Flush()
}
