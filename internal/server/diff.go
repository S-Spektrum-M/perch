package server

import (
	"encoding/json"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// DiffLine contains text, never trusted HTML. Templates escape file contents.
type DiffLine struct {
	Kind string
	Text string
}

func diffLines(diff string) []DiffLine {
	if diff == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	out := make([]DiffLine, 0, len(lines))
	for _, line := range lines {
		kind := "context"
		switch {
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "@@"):
			kind = "header"
		case strings.HasPrefix(line, "+"):
			kind = "add"
		case strings.HasPrefix(line, "-"):
			kind = "remove"
		}
		out = append(out, DiffLine{Kind: kind, Text: line})
	}
	return out
}

// This is a preview of the requested replacement, not proof it was applied.
// Actual tool-result diffs (including their file line numbers) take precedence
// in the result card. Preview hunk numbers are relative to the supplied snippet.
func editPreview(name string, raw json.RawMessage) []DiffLine {
	if name != "edit" {
		return nil
	}
	type replacement struct {
		OldText string `json:"oldText"`
		NewText string `json:"newText"`
	}
	var args struct {
		replacement
		Edits []replacement `json:"edits"`
	}
	if json.Unmarshal(raw, &args) != nil {
		return nil
	}
	edits := args.Edits
	if len(edits) == 0 {
		edits = []replacement{args.replacement}
	}
	var result []DiffLine
	for _, edit := range edits {
		// Avoid quadratic matching work for unusually large replacements.
		if len(edit.OldText)+len(edit.NewText) > 100_000 {
			return nil // retain the ordinary truncated arguments view
		}
		if edit.OldText == edit.NewText {
			continue
		}
		// Normalize final newlines for line matching; report their absence separately.
		split := func(s string) []string {
			if s == "" {
				return nil
			}
			lines := strings.SplitAfter(strings.TrimSuffix(s, "\n")+"\n", "\n")
			return lines[:len(lines)-1]
		}
		diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: split(edit.OldText), B: split(edit.NewText),
			FromFile: "before (snippet)", ToFile: "after (snippet)", Context: 3,
		})
		if err == nil {
			result = append(result, diffLines(diff)...)
			if edit.OldText != "" && !strings.HasSuffix(edit.OldText, "\n") {
				result = append(result, DiffLine{Kind: "header", Text: "\\ No newline at end of before snippet"})
			}
			if edit.NewText != "" && !strings.HasSuffix(edit.NewText, "\n") {
				result = append(result, DiffLine{Kind: "header", Text: "\\ No newline at end of after snippet"})
			}
		}
	}
	return result
}
