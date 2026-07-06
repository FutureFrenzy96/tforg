package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

type config struct {
	check   bool
	quiet   bool
	fmtOnly bool
	dest    map[string]string
}

// span is a half-open byte range [start, end) within a source file.
type span struct{ start, end int }

// moveText is a block (with its attached leading comments) extracted from a
// source file, destined for another file in the same directory.
type moveText struct {
	text []byte
	from string
	desc string
}

// dirOutcome is the computed result for one directory. Nothing is written to
// disk until applyOutcome runs.
type dirOutcome struct {
	dir     string
	writes  map[string][]byte // base name -> new file content
	creates map[string]bool   // subset of writes that are new files
	deletes map[string]bool   // files emptied by moves
	moves   []string          // human-readable move descriptions
	fmtOnly []string          // files changed by formatting alone
	errs    []string
}

func (o *dirOutcome) changed() bool {
	return len(o.writes) > 0 || len(o.deletes) > 0
}

// processDir computes the reorganization and formatting of the targeted files
// (base names, sorted) within a single directory. Blocks only ever move
// between files in the same directory, since a Terraform module is a
// directory.
func processDir(dir string, bases []string, cfg config) dirOutcome {
	out := dirOutcome{
		dir:     dir,
		writes:  map[string][]byte{},
		creates: map[string]bool{},
		deletes: map[string]bool{},
	}

	type fileState struct {
		base     string
		src      []byte
		removals []span
		parseErr bool
	}

	var states []*fileState
	srcs := map[string][]byte{}
	appends := map[string][]moveText{}

	for _, base := range bases {
		path := filepath.Join(dir, base)
		src, err := os.ReadFile(path)
		if err != nil {
			out.errs = append(out.errs, err.Error())
			continue
		}
		st := &fileState{base: base, src: src}
		states = append(states, st)
		srcs[base] = src

		f, diags := hclsyntax.ParseConfig(src, path, hcl.InitialPos)
		if diags.HasErrors() {
			for _, d := range diags.Errs() {
				out.errs = append(out.errs, d.Error())
			}
			st.parseErr = true
			continue
		}
		if cfg.fmtOnly || isOverride(base) {
			continue
		}

		body := f.Body.(*hclsyntax.Body)
		if len(body.Blocks) == 0 {
			continue
		}
		flags, lineStarts := lineFlags(src, path)
		for _, blk := range body.Blocks {
			dest := cfg.dest[blk.Type]
			if dest == "" || strings.EqualFold(dest, base) {
				continue
			}
			sp := blockSpan(src, blk, lineStarts, flags)
			st.removals = append(st.removals, sp)
			appends[dest] = append(appends[dest], moveText{
				text: src[sp.start:sp.end],
				from: base,
				desc: blockDesc(blk),
			})
			out.moves = append(out.moves, fmt.Sprintf("%s: %s → %s", base, blockDesc(blk), dest))
		}
	}

	// Destination files that exist on disk but were not in the target set
	// (single-file mode) must be read, and must parse, before we append to
	// them. If any destination is unusable, abort all moves in this
	// directory rather than leave it half-reorganized.
	destOnDisk := map[string][]byte{}
	abort := false
	for dest := range appends {
		if src, ok := srcs[dest]; ok {
			_ = src
			for _, st := range states {
				if st.base == dest && st.parseErr {
					out.errs = append(out.errs, fmt.Sprintf("%s: cannot move blocks into a file with syntax errors", filepath.Join(dir, dest)))
					abort = true
				}
			}
			continue
		}
		path := filepath.Join(dir, dest)
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			out.errs = append(out.errs, err.Error())
			abort = true
			continue
		}
		if _, diags := hclsyntax.ParseConfig(b, path, hcl.InitialPos); diags.HasErrors() {
			out.errs = append(out.errs, fmt.Sprintf("%s: cannot move blocks into a file with syntax errors", path))
			abort = true
			continue
		}
		destOnDisk[dest] = b
	}
	if abort {
		appends = nil
		out.moves = nil
		for _, st := range states {
			st.removals = nil
		}
	}

	// Assemble final (pre-format) content: sources minus removed blocks,
	// then destinations plus appended blocks.
	final := map[string][]byte{}
	for _, st := range states {
		if st.parseErr {
			continue // never touch a file that does not parse
		}
		if len(st.removals) > 0 {
			final[st.base] = removeSpans(st.src, st.removals)
		} else {
			final[st.base] = st.src
		}
	}
	for dest, texts := range appends {
		base, ok := final[dest]
		if !ok {
			base = destOnDisk[dest] // nil for a brand-new file
		}
		final[dest] = appendTexts(base, texts)
	}

	removedFrom := map[string]bool{}
	for _, st := range states {
		if len(st.removals) > 0 {
			removedFrom[st.base] = true
		}
	}

	for base, content := range final {
		orig, existed := srcs[base]
		if !existed {
			if b, ok := destOnDisk[base]; ok {
				orig, existed = b, true
			}
		}
		if existed && len(bytes.TrimSpace(content)) == 0 {
			out.deletes[base] = true
			continue
		}
		formatted := hclwrite.Format(content)
		switch {
		case !existed:
			out.writes[base] = formatted
			out.creates[base] = true
		case !bytes.Equal(formatted, orig):
			out.writes[base] = formatted
			if len(appends[base]) == 0 && !removedFrom[base] {
				out.fmtOnly = append(out.fmtOnly, base)
			}
		}
	}
	sort.Strings(out.fmtOnly)
	return out
}

// applyOutcome writes the computed changes to disk.
func applyOutcome(o dirOutcome) []string {
	var errs []string
	for base, content := range o.writes {
		path := filepath.Join(o.dir, base)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for base := range o.deletes {
		if err := os.Remove(filepath.Join(o.dir, base)); err != nil {
			errs = append(errs, err.Error())
		}
	}
	return errs
}

// lineFlag classifies one source line so leading comments can be attached to
// the block that follows them.
type lineFlag struct {
	hasCode    bool
	hasComment bool
}

func (f lineFlag) commentOnly() bool { return f.hasComment && !f.hasCode }

// lineFlags lexes the file and marks, per line, whether it carries code
// and/or comments. Using the real HCL lexer means heredoc bodies and string
// contents that merely look like comments are correctly treated as code.
func lineFlags(src []byte, name string) ([]lineFlag, []int) {
	lineStarts := []int{0}
	for i, b := range src {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}
	flags := make([]lineFlag, len(lineStarts))

	toks, _ := hclsyntax.LexConfig(src, name, hcl.InitialPos)
	for _, tok := range toks {
		if tok.Type == hclsyntax.TokenNewline || tok.Type == hclsyntax.TokenEOF {
			continue
		}
		s, e := tok.Range.Start.Line, tok.Range.End.Line
		// Tokens that swallow their trailing newline (line comments,
		// heredoc lines) report their end at column 1 of the next line.
		if e > s && tok.Range.End.Column == 1 {
			e--
		}
		if e > len(flags) {
			e = len(flags)
		}
		for l := s; l <= e; l++ {
			if tok.Type == hclsyntax.TokenComment {
				flags[l-1].hasComment = true
			} else {
				flags[l-1].hasCode = true
			}
		}
	}
	return flags, lineStarts
}

// blockSpan returns the byte range of a top-level block, extended upward over
// contiguous comment-only lines (which conventionally document the block) and
// downward to the end of the closing-brace line so a trailing same-line
// comment travels with it.
func blockSpan(src []byte, blk *hclsyntax.Block, lineStarts []int, flags []lineFlag) span {
	line := blk.TypeRange.Start.Line // 1-based
	for line > 1 && flags[line-2].commentOnly() {
		line--
	}
	start := lineStarts[line-1]

	end := blk.CloseBraceRange.End.Byte
	for end < len(src) && src[end] != '\n' {
		end++
	}
	if end < len(src) {
		end++ // include the newline
	}
	return span{start, end}
}

// removeSpans returns src with the given block spans cut out. Blank-line runs
// are normalized only at the cut seams; untouched content (including heredoc
// bodies) is preserved byte for byte.
func removeSpans(src []byte, spans []span) []byte {
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var segs [][]byte
	prev := 0
	for _, sp := range spans {
		segs = append(segs, src[prev:sp.start])
		prev = sp.end
	}
	segs = append(segs, src[prev:])

	var out []byte
	for _, seg := range segs {
		if len(bytes.TrimSpace(seg)) == 0 {
			continue
		}
		if len(out) == 0 {
			out = append(out, bytes.TrimLeft(seg, "\r\n")...)
		} else {
			out = bytes.TrimRight(out, "\r\n")
			out = append(out, '\n', '\n')
			out = append(out, bytes.TrimLeft(seg, "\r\n")...)
		}
	}
	if len(out) > 0 {
		out = bytes.TrimRight(out, "\r\n")
		out = append(out, '\n')
	}
	return out
}

// appendTexts appends moved blocks to a destination file's content with
// exactly one blank line between top-level items.
func appendTexts(base []byte, texts []moveText) []byte {
	var out []byte
	if len(bytes.TrimSpace(base)) > 0 {
		out = bytes.TrimRight(base, "\r\n")
	}
	for _, t := range texts {
		txt := bytes.Trim(t.text, "\r\n")
		if len(out) > 0 {
			out = append(out, '\n', '\n')
		}
		out = append(out, txt...)
	}
	out = append(out, '\n')
	return out
}

func blockDesc(blk *hclsyntax.Block) string {
	parts := []string{blk.Type}
	for _, l := range blk.Labels {
		parts = append(parts, fmt.Sprintf("%q", l))
	}
	return strings.Join(parts, " ")
}

// isOverride reports whether the file uses Terraform's override semantics;
// those files must keep their blocks in place to preserve merge behavior.
func isOverride(base string) bool {
	n := strings.ToLower(base)
	return n == "override.tf" || strings.HasSuffix(n, "_override.tf")
}
