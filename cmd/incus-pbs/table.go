package main

import (
	"io"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// newTable returns a light-bordered table that renders to w. Header cells are
// printed as given; columns named SIZE are right-aligned.
func newTable(w io.Writer, header ...any) table.Writer {
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	if len(header) > 0 {
		t.AppendHeader(table.Row(header))
	}
	t.SetColumnConfigs([]table.ColumnConfig{{Name: "SIZE", Align: text.AlignRight, AlignHeader: text.AlignRight}})
	return t
}
