// Package synctex reads the .synctex.gz file a TeX engine emits and answers the
// two questions a writer actually asks: "where did this line end up in the PDF?"
// and "which line produced this spot on the page?".
//
// The format is finicky and the coordinate conventions are easy to get subtly
// wrong, which is why this lives behind a small interface with its own tests
// rather than being spread through the app.
//
// # The format
//
// A gzip-compressed text file: a preamble of key:value lines, then a Content
// section of one-character-tagged records, then a postamble. Records look like
//
//	(1,18:4736287,46220575:26673152,41484288,0
//	 │ │  │       │        └ width,height,depth
//	 │ │  │       └ y, downward from the top of the page
//	 │ │  └ x, rightward from the left edge
//	 │ └ source line
//	 └ input tag, resolved against the Input: records in the preamble
//
// # Units
//
// Coordinates are in scaled points. TeX's point is 1/72.27 inch while PDF's is
// 1/72 inch, so converting to PDF coordinates is not simply a division by 65536
// — getting that wrong puts everything out by 0.375%, which is invisible on a
// short line and a whole line's worth of error down a page.
package synctex

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ErrInputTooLarge means a bounded SyncTeX parse exceeded its decompressed
// byte budget.
var ErrInputTooLarge = errors.New("synctex: input exceeds size limit")

// spPerPDFPoint converts scaled points to PDF points (big points).
//
//	1 TeX point = 1/72.27 inch, 1 PDF point = 1/72 inch, 1 TeX point = 65536 sp
//	=> sp per PDF point = 65536 * 72.27/72
const spPerPDFPoint = 65781.76

// Kind is a record's type character, as it appears in the file.
type Kind byte

// The record types SyncTeX uses. Only some carry an extent worth pointing at.
const (
	KindHBox    Kind = '(' // horizontal box, the workhorse for text
	KindVBox    Kind = '[' // vertical box
	KindVoidH   Kind = 'h'
	KindVoidV   Kind = 'v'
	KindGlue    Kind = 'g'
	KindKern    Kind = 'k'
	KindMath    Kind = '$'
	KindRule    Kind = 'r'
	KindCurrent Kind = 'x'
)

// Record is one positioned element, in PDF points with the origin at the page's
// top-left corner.
type Record struct {
	Kind Kind
	Tag  int
	Line int
	Page int

	// X, Y locate the element's reference point. Y grows downward.
	X, Y float64
	// Width, Height, Depth describe the box around it. Height is above the
	// reference point, Depth below.
	Width, Height, Depth float64
}

// Rect is the area a record occupies, in PDF points from the page's top-left.
type Rect struct {
	Page           int     `json:"page"`
	X              float64 `json:"x"`
	Y              float64 `json:"y"`
	Width          float64 `json:"width"`
	Height         float64 `json:"height"`
	SourceLine     int     `json:"sourceLine"`
	SourceFilePath string  `json:"sourceFilePath"`
}

// rect turns a record's reference point and box metrics into an area. TeX
// measures height up from the baseline and depth down from it.
func (r Record) rect() Rect {
	top := r.Y - r.Height
	h := r.Height + r.Depth
	// Glue, kerns and void boxes carry no extent. Give them a thin band so a
	// highlight drawn on them is still visible and hit-testing still works.
	if h <= 0 {
		top = r.Y - 6
		h = 8
	}
	w := r.Width
	if w <= 0 {
		w = 4
	}
	return Rect{Page: r.Page, X: r.X, Y: top, Width: w, Height: h, SourceLine: r.Line}
}

// File is a parsed .synctex.gz.
type File struct {
	Version string

	// Inputs maps an input tag to the path the engine recorded. Tectonic writes
	// absolute paths; other engines write paths relative to the compile
	// directory. Callers resolve them against the project.
	Inputs map[int]string

	// Records are grouped by page, in file order.
	Pages map[int][]Record

	// PageCount is the highest page number seen.
	PageCount int
}

// ParseFile reads a .synctex or .synctex.gz file.
func ParseFile(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("synctex: open: %w", err)
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("synctex: gunzip %s: %w", path, err)
		}
		defer zr.Close()
		r = zr
	}
	return Parse(r)
}

// ParseGzipBounded parses a gzip-compressed SyncTeX stream after enforcing a
// decompressed-size budget. The helper uses this for attacker-influenced
// project output; the legacy Wails file parser retains its existing behavior.
func ParseGzipBounded(input io.Reader, maximumBytes int64) (*File, error) {
	if maximumBytes <= 0 {
		return nil, ErrInputTooLarge
	}
	compressed, err := gzip.NewReader(input)
	if err != nil {
		return nil, fmt.Errorf("synctex: gunzip: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(compressed, maximumBytes+1))
	closeErr := compressed.Close()
	if readErr != nil {
		return nil, fmt.Errorf("synctex: read compressed stream: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("synctex: close compressed stream: %w", closeErr)
	}
	if int64(len(data)) > maximumBytes {
		return nil, ErrInputTooLarge
	}
	return Parse(bytes.NewReader(data))
}

// Parse reads an uncompressed SyncTeX stream.
func Parse(r io.Reader) (*File, error) {
	out := &File{
		Inputs: make(map[int]string),
		Pages:  make(map[int][]Record),
	}

	// Preamble scaling. Defaults match what every engine we have seen emits, so
	// a file missing them is still usable.
	unit, xOffset, yOffset := 1.0, 0.0, 0.0

	scanner := bufio.NewScanner(r)
	// Records are short, but a pathological line should not kill the parse.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	inContent := false
	page := 0

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			continue
		}

		if !inContent {
			switch {
			case line == "Content:":
				inContent = true
			case strings.HasPrefix(line, "SyncTeX Version:"):
				out.Version = strings.TrimPrefix(line, "SyncTeX Version:")
			case strings.HasPrefix(line, "Input:"):
				tag, path, ok := parseInput(line)
				// Engines emit placeholder Input records with no path for files
				// they synthesised; those are not anything a writer can open.
				if ok && path != "" {
					out.Inputs[tag] = path
				}
			case strings.HasPrefix(line, "Unit:"):
				unit = parseFloatDefault(strings.TrimPrefix(line, "Unit:"), 1)
			case strings.HasPrefix(line, "X Offset:"):
				xOffset = parseFloatDefault(strings.TrimPrefix(line, "X Offset:"), 0)
			case strings.HasPrefix(line, "Y Offset:"):
				yOffset = parseFloatDefault(strings.TrimPrefix(line, "Y Offset:"), 0)
			}
			continue
		}

		// Input records are not confined to the preamble. The engine emits one
		// wherever it first opens a file, so a chapter that starts on page two has
		// its Input record buried in the middle of the content — after the `}1`
		// that ends page one. Reading only the preamble made every such file
		// invisible to both searches, which looked exactly like "SyncTeX does not
		// work for that chapter".
		if strings.HasPrefix(line, "Input:") {
			if tag, path, ok := parseInput(line); ok && path != "" {
				out.Inputs[tag] = path
			}
			continue
		}

		switch line[0] {
		case '{', '<': // page or sheet begins
			if n, err := strconv.Atoi(line[1:]); err == nil {
				page = n
				if n > out.PageCount {
					out.PageCount = n
				}
			}
			continue
		case '}', '>': // page ends
			page = 0
			continue
		case ']', ')': // box ends; carries no position
			continue
		case '!': // byte-offset anchor for random access; not a position
			continue
		}
		if strings.HasPrefix(line, "Post") || strings.HasPrefix(line, "Count") {
			// Postamble; nothing positional left.
			continue
		}

		rec, ok := parseRecord(line, page, unit, xOffset, yOffset)
		if !ok {
			continue
		}
		out.Pages[rec.Page] = append(out.Pages[rec.Page], rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("synctex: read: %w", err)
	}
	if !inContent {
		return nil, fmt.Errorf("synctex: no Content section; not a SyncTeX file")
	}
	return out, nil
}

func parseInput(line string) (tag int, path string, ok bool) {
	// Input:<tag>:<path>, and the path may itself contain colons.
	rest := strings.TrimPrefix(line, "Input:")
	tagField, path, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, "", false
	}
	tag, err := strconv.Atoi(tagField)
	if err != nil {
		return 0, "", false
	}
	return tag, strings.TrimSpace(path), true
}

func parseFloatDefault(s string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v == 0 {
		return def
	}
	return v
}

// parseRecord reads one positional record:
//
//	<kind><tag>,<line>[,<column>]:<x>,<y>[:<width>,<height>,<depth>|:<width>]
func parseRecord(line string, page int, unit, xOffset, yOffset float64) (Record, bool) {
	kind := Kind(line[0])
	switch kind {
	case KindHBox, KindVBox, KindVoidH, KindVoidV, KindGlue, KindKern, KindMath, KindRule, KindCurrent:
	default:
		return Record{}, false
	}
	if page == 0 {
		// A positional record outside any page cannot be pointed at.
		return Record{}, false
	}

	fields := strings.Split(line[1:], ":")
	if len(fields) < 2 {
		return Record{}, false
	}

	// tag,line[,column]
	ids := strings.Split(fields[0], ",")
	if len(ids) < 2 {
		return Record{}, false
	}
	tag, err1 := strconv.Atoi(ids[0])
	srcLine, err2 := strconv.Atoi(ids[1])
	if err1 != nil || err2 != nil {
		return Record{}, false
	}

	// x,y
	coords := strings.Split(fields[1], ",")
	if len(coords) < 2 {
		return Record{}, false
	}
	x, err1 := strconv.ParseFloat(coords[0], 64)
	y, err2 := strconv.ParseFloat(coords[1], 64)
	if err1 != nil || err2 != nil {
		return Record{}, false
	}

	toPDF := func(v, offset float64) float64 { return (v*unit + offset) / spPerPDFPoint }

	rec := Record{
		Kind: kind,
		Tag:  tag,
		Line: srcLine,
		Page: page,
		X:    toPDF(x, xOffset),
		Y:    toPDF(y, yOffset),
	}

	// width[,height,depth]. Kerns and glue carry only a width, boxes carry all
	// three, and some records carry none.
	if len(fields) >= 3 {
		dims := strings.Split(fields[2], ",")
		if v, err := strconv.ParseFloat(dims[0], 64); err == nil {
			rec.Width = v * unit / spPerPDFPoint
		}
		if len(dims) >= 3 {
			if v, err := strconv.ParseFloat(dims[1], 64); err == nil {
				rec.Height = v * unit / spPerPDFPoint
			}
			if v, err := strconv.ParseFloat(dims[2], 64); err == nil {
				rec.Depth = v * unit / spPerPDFPoint
			}
		}
	}
	return rec, true
}

// TagFor returns the input tag whose recorded path matches, using the caller's
// comparison so path resolution stays the app's business.
func (f *File) TagFor(match func(recordedPath string) bool) (int, bool) {
	// Deterministic: lowest tag wins when several paths match.
	best, found := 0, false
	for tag, path := range f.Inputs {
		if !match(path) {
			continue
		}
		if !found || tag < best {
			best, found = tag, true
		}
	}
	return best, found
}

// Forward answers "where did this line end up?".
//
// It returns the areas produced by the given line, which may be several — a line
// that wraps, or one spanning a page break. Empty when the line produced no
// output at all, which is normal: comments, blank lines and preamble lines have
// no place on a page.
//
// A line with no records of its own falls forward to the next line that has
// some, so putting the cursor on a blank line between paragraphs still lands
// somewhere sensible rather than nowhere.
func (f *File) Forward(tag, line int) []Rect {
	if rects := f.rectsFor(tag, line); len(rects) > 0 {
		return rects
	}

	// The nearest line we can actually point at, preferring later over earlier:
	// the writer is usually about to type there.
	//
	// It has to be a line with *material* on the page, not merely one with
	// records. A line frequently carries nothing but kerns and glue, because TeX
	// attributes a paragraph's text to the line that ends it — so falling back to
	// "the nearest line that appears in the file" lands on another empty line and
	// reports nothing, which is how a whole paragraph ends up unreachable.
	best, bestScore := 0, math.MaxInt32
	for _, records := range f.Pages {
		for _, r := range records {
			if r.Tag != tag || r.Line == line || !isTextish(r.Kind) {
				continue
			}
			// Doubling the distance leaves the odd numbers for lines before the
			// cursor, so a tie resolves forward.
			score := (r.Line - line) * 2
			if r.Line < line {
				score = (line-r.Line)*2 + 1
			}
			if score < bestScore {
				best, bestScore = r.Line, score
			}
		}
	}
	if best == 0 {
		return nil
	}
	return f.rectsFor(tag, best)
}

// isTextish reports whether a record marks actual material on the page, as
// opposed to structure or spacing.
//
// The distinction matters more than it looks. Every page sits inside a vbox that
// spans the whole text area and is attributed to *some* source line; treating
// that as a candidate makes a highlight cover the entire page, and makes a click
// anywhere report whichever line happened to own the page box. Glue and kerns are
// the opposite problem: positions between things, with no extent worth pointing
// at.
func isTextish(k Kind) bool {
	switch k {
	case KindHBox, KindVoidH, KindVoidV, KindMath, KindRule:
		return true
	}
	return false
}

// candidatesOn returns the records worth pointing at on a page, falling back to
// structural boxes only when there is genuinely nothing else.
func (f *File) candidatesOn(page int) []Record {
	records := f.Pages[page]
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if isTextish(r.Kind) {
			out = append(out, r)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, r := range records {
		if r.Kind != KindGlue && r.Kind != KindKern {
			out = append(out, r)
		}
	}
	return out
}

// rectsFor collects the areas for an exact tag and line, in page then reading
// order.
//
// A source line can produce a great deal of output — \lipsum, or a macro
// expanding to paragraphs — and all of it is returned: the area a line covers is
// what the writer asked to see. Each row comes back as its own band rather than
// one bounding box, so the highlight traces the text instead of boxing in the
// whitespace around it.
func (f *File) rectsFor(tag, line int) []Rect {
	byPage := map[int][]Rect{}

	for page := 1; page <= f.PageCount; page++ {
		var matches []Rect
		for _, r := range f.candidatesOn(page) {
			if r.Tag == tag && r.Line == line {
				matches = append(matches, r.rect())
			}
		}
		if len(matches) == 0 {
			continue
		}

		bands := bandsIn(matches)
		for i := range bands {
			bands[i].SourceLine = line
		}
		byPage[page] = bands
	}

	if len(byPage) == 0 {
		return nil
	}
	var out []Rect
	for page := 1; page <= f.PageCount; page++ {
		out = append(out, byPage[page]...)
	}
	return out
}

// bandsIn turns a source line's boxes on one page into the rows it occupies.
//
// Two problems are solved here, and they pull in opposite directions.
//
// The first is spillover. A line's boxes are not necessarily one run of text.
// When a file is \input, TeX first finishes the paragraph already in progress
// and attributes those broken-off lines to the *new* file's first line — so
// \section{Method} on line 1 of a chapter owns nine lines of the previous
// chapter's paragraph as well as its own heading. Pointing at all of that would
// send the writer inches above the heading they asked for. So the boxes are
// split into clusters separated by vertical gaps and only the last is kept.
//
// The second is extent. Within that cluster the writer wants the whole area
// their line produced, not just where it starts: a wrapped sentence is three
// rows, a paragraph is ten. So every row of the chosen cluster is returned,
// each one widened across the boxes sitting beside it. Drawn together they
// read like a text selection rather than a single box.
func bandsIn(matches []Rect) []Rect {
	sorted := make([]Rect, len(matches))
	copy(sorted, matches)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Y < sorted[j].Y })

	// Start of the final cluster. Consecutive rows of the same block are a few
	// points apart; a new block is separated by at least a line's worth of space.
	start := 0
	for i := 1; i < len(sorted); i++ {
		prev := sorted[i-1]
		gap := sorted[i].Y - (prev.Y + prev.Height)
		if gap > prev.Height*1.6+2 {
			start = i
		}
	}

	var bands []Rect
	// seed is the first box of the row being built. Overlap is tested against it
	// rather than against the growing union, so one box with an unusually deep
	// descender cannot stretch a row until it swallows the next one.
	var seed Rect
	for _, m := range sorted[start:] {
		if len(bands) > 0 && m.Y < seed.Y+seed.Height && m.Y+m.Height > seed.Y {
			bands[len(bands)-1] = union(bands[len(bands)-1], m)
			continue
		}
		bands = append(bands, m)
		seed = m
	}
	return bands
}

func union(a, b Rect) Rect {
	right := math.Max(a.X+a.Width, b.X+b.Width)
	bottom := math.Max(a.Y+a.Height, b.Y+b.Height)
	x := math.Min(a.X, b.X)
	y := math.Min(a.Y, b.Y)
	return Rect{Page: a.Page, X: x, Y: y, Width: right - x, Height: bottom - y}
}

// Hit is the source location behind a point on a page.
type Hit struct {
	Tag  int
	Line int
	// Distance is 0 when the point landed inside the matched area.
	Distance float64
	// Rect is the area that matched, so the UI can show what it resolved to
	// rather than making the writer guess whether the jump was sensible.
	Rect Rect
}

// Inverse answers "which line produced this spot?", for a point in PDF points
// from the page's top-left.
//
// Vertical distance decides first, then horizontal, then the smaller box.
// Straight-line distance is the obvious choice and the wrong one: a click in the
// gap between two lines is a few points from both, but an enclosing box contains
// it outright, so plain distance answers with whichever source line happens to
// own the page's vbox. Clicking near a line means you meant that line.
func (f *File) Inverse(page int, x, y float64) (Hit, bool) {
	candidates := f.candidatesOn(page)
	if len(candidates) == 0 {
		return Hit{}, false
	}

	var best Hit
	bestDV, bestDH, bestArea := math.Inf(1), math.Inf(1), math.Inf(1)
	found := false

	for _, r := range candidates {
		rect := r.rect()
		dv := axisDistance(rect.Y, rect.Y+rect.Height, y)
		dh := axisDistance(rect.X, rect.X+rect.Width, x)
		area := rect.Width * rect.Height

		const epsilon = 0.001
		better := dv < bestDV-epsilon ||
			(math.Abs(dv-bestDV) <= epsilon && dh < bestDH-epsilon) ||
			(math.Abs(dv-bestDV) <= epsilon && math.Abs(dh-bestDH) <= epsilon && area < bestArea)
		if !found || better {
			rect.SourceLine = r.Line
			best = Hit{Tag: r.Tag, Line: r.Line, Distance: math.Hypot(dh, dv), Rect: rect}
			bestDV, bestDH, bestArea = dv, dh, area
			found = true
		}
	}
	return best, found
}

// axisDistance is how far v lies outside the span [lo, hi], or 0 if inside.
func axisDistance(lo, hi, v float64) float64 {
	return math.Max(0, math.Max(lo-v, v-hi))
}
