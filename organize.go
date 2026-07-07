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
	sort    bool
	diff    bool
	dest    map[string]string
	rules   []placeRule
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

// moveEvent records one block relocation for reporting.
type moveEvent struct {
	from, dest, desc string
}

// dirOutcome is the computed result for one directory. Nothing is written to
// disk until applyOutcome runs.
type dirOutcome struct {
	dir     string
	writes  map[string][]byte // base name -> new file content
	creates map[string]bool   // subset of writes that are new files
	deletes map[string]bool   // files emptied by moves
	origs   map[string][]byte // previous content of written/deleted files
	moves   []moveEvent
	fmtOnly []string // files changed by formatting alone
	errs    []string
}

// fileState is one targeted file's parsed form during processDir.
type fileState struct {
	base       string
	src        []byte
	blocks     []*hclsyntax.Block
	flags      []lineFlag
	lineStarts []int
	keepBlocks bool // override files and -fmt-only: format, never move
	parseErr   bool
	removals   []span
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
		origs:   map[string][]byte{},
	}

	var states []*fileState
	srcs := map[string][]byte{}

	for _, base := range bases {
		path := filepath.Join(dir, base)
		src, err := os.ReadFile(path)
		if err != nil {
			out.errs = append(out.errs, err.Error())
			continue
		}
		st := &fileState{base: base, src: src, keepBlocks: cfg.fmtOnly || isOverride(base)}
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
		if st.keepBlocks {
			continue
		}
		st.blocks = f.Body.(*hclsyntax.Body).Blocks
		if len(st.blocks) > 0 {
			st.flags, st.lineStarts = lineFlags(src, path)
		}
	}

	// Duplicate addresses (two `variable "region"`, two identical resources,
	// ...) are Terraform errors that would only surface at plan time, and
	// merging the copies into one file would make the mess harder to untangle.
	// Report them and keep every block where it is.
	abort := false
	if dups := findDuplicates(states); len(dups) > 0 {
		for _, d := range dups {
			out.errs = append(out.errs, fmt.Sprintf("%s: %s", dir, d))
		}
		abort = true
	}

	appends := map[string][]moveText{}
	if !abort {
		for _, st := range states {
			if st.keepBlocks || st.parseErr {
				continue
			}
			for _, blk := range st.blocks {
				dest, keep := cfg.destFor(blk)
				if keep || dest == "" || strings.EqualFold(dest, st.base) {
					continue
				}
				sp := blockSpan(st.src, blk, st.lineStarts, st.flags)
				st.removals = append(st.removals, sp)
				appends[dest] = append(appends[dest], moveText{
					text: st.src[sp.start:sp.end],
					from: st.base,
					desc: blockDesc(blk),
				})
				out.moves = append(out.moves, moveEvent{from: st.base, dest: dest, desc: blockDesc(blk)})
			}
		}
	}

	// Destination files that exist on disk but were not in the target set
	// (single-file mode) must be read, must parse, and must not already
	// define a block we are about to move in. If any destination is
	// unusable, abort all moves in this directory rather than leave it
	// half-reorganized.
	destOnDisk := map[string][]byte{}
	for dest, texts := range appends {
		_ = texts
		if _, ok := srcs[dest]; ok {
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
		f, diags := hclsyntax.ParseConfig(b, path, hcl.InitialPos)
		if diags.HasErrors() {
			out.errs = append(out.errs, fmt.Sprintf("%s: cannot move blocks into a file with syntax errors", path))
			abort = true
			continue
		}
		existing := map[string]bool{}
		for _, blk := range f.Body.(*hclsyntax.Body).Blocks {
			if n, ok := uniqueBlockLabels[blk.Type]; ok && len(blk.Labels) == n {
				existing[blockDesc(blk)] = true
			}
		}
		for _, t := range texts {
			if existing[t.desc] {
				out.errs = append(out.errs, fmt.Sprintf("%s: duplicate %s (%s, %s)", dir, t.desc, t.from, dest))
				abort = true
			}
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

	if cfg.sort && !cfg.fmtOnly {
		keep := map[string]bool{}
		for _, st := range states {
			keep[st.base] = st.keepBlocks
		}
		for base, content := range final {
			if keep[base] {
				continue // override files keep their block order
			}
			final[base] = sortBlocks(content, filepath.Join(dir, base))
		}
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
			out.origs[base] = orig
			continue
		}
		formatted := hclwrite.Format(content)
		switch {
		case !existed:
			out.writes[base] = formatted
			out.creates[base] = true
		case !bytes.Equal(formatted, orig):
			out.writes[base] = formatted
			out.origs[base] = orig
			if len(appends[base]) == 0 && !removedFrom[base] {
				out.fmtOnly = append(out.fmtOnly, base)
			}
		}
	}
	sort.Strings(out.fmtOnly)
	return out
}

// uniqueBlockLabels lists the block types whose full address must be unique
// within a module, with the label count that forms the address. Types that
// Terraform merges or allows repeated (provider aliases, terraform, locals,
// moved, import) are deliberately absent.
var uniqueBlockLabels = map[string]int{
	"resource":  2,
	"data":      2,
	"ephemeral": 2,
	"variable":  1,
	"output":    1,
	"module":    1,
	"check":     1,
}

// findDuplicates reports block addresses defined more than once across the
// parsed files of a directory. Override files are exempt: duplicating an
// address is exactly how Terraform's override mechanism works.
func findDuplicates(states []*fileState) []string {
	seen := map[string][]string{}
	var order []string
	for _, st := range states {
		if st.keepBlocks || st.parseErr {
			continue
		}
		for _, blk := range st.blocks {
			n, ok := uniqueBlockLabels[blk.Type]
			if !ok || len(blk.Labels) != n {
				continue
			}
			key := blockDesc(blk)
			if len(seen[key]) == 0 {
				order = append(order, key)
			}
			seen[key] = append(seen[key], st.base)
		}
	}
	var msgs []string
	for _, key := range order {
		if files := seen[key]; len(files) > 1 {
			msgs = append(msgs, fmt.Sprintf("duplicate %s (%s)", key, strings.Join(files, ", ")))
		}
	}
	return msgs
}

// sortableTypes are the block types -sort may alphabetize; reordering
// resources or modules is riskier and intentionally out of scope.
var sortableTypes = map[string]bool{"variable": true, "output": true}

// sortBlocks alphabetizes the blocks of a file by label when it is safe to do
// so: every block must be the same sortable type, and nothing but whitespace
// may live outside the blocks (a stray standalone comment would lose its
// context when neighbors move). Attached leading comments travel with their
// block, as in a move.
func sortBlocks(content []byte, path string) []byte {
	f, diags := hclsyntax.ParseConfig(content, path, hcl.InitialPos)
	if diags.HasErrors() {
		return content
	}
	blocks := f.Body.(*hclsyntax.Body).Blocks
	if len(blocks) < 2 || !sortableTypes[blocks[0].Type] {
		return content
	}
	for _, b := range blocks {
		if b.Type != blocks[0].Type || len(b.Labels) == 0 {
			return content
		}
	}
	flags, lineStarts := lineFlags(content, path)
	type item struct {
		label string
		text  []byte
	}
	items := make([]item, len(blocks))
	spans := make([]span, len(blocks))
	for i, b := range blocks {
		spans[i] = blockSpan(content, b, lineStarts, flags)
		items[i] = item{label: b.Labels[0], text: content[spans[i].start:spans[i].end]}
	}
	if len(bytes.TrimSpace(outsideSpans(content, spans))) > 0 {
		return content
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].label < items[j].label })
	var out []byte
	for _, it := range items {
		if len(out) > 0 {
			out = append(out, '\n')
		}
		out = append(out, bytes.Trim(it.text, "\r\n")...)
		out = append(out, '\n')
	}
	return out
}

// outsideSpans returns everything in src not covered by the given spans.
func outsideSpans(src []byte, spans []span) []byte {
	sorted := append([]span(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	var out []byte
	prev := 0
	for _, sp := range sorted {
		out = append(out, src[prev:sp.start]...)
		prev = sp.end
	}
	return append(out, src[prev:]...)
}

// applyOutcome writes the computed changes to disk. Writes are atomic
// (temp file + rename) so an interrupted run can never leave a truncated
// .tf file behind.
func applyOutcome(o dirOutcome) []string {
	var errs []string
	for base, content := range o.writes {
		path := filepath.Join(o.dir, base)
		perm := os.FileMode(0o644)
		if fi, err := os.Stat(path); err == nil {
			perm = fi.Mode().Perm()
		}
		if err := atomicWrite(path, content, perm); err != nil {
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

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tforg-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename has happened
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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
