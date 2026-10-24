package synctex

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGzipBoundedParsesWithinLimitAndRejectsOversizeInput(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "multipage.synctex.gz"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseGzipBounded(bytes.NewReader(data), 32*1024*1024)
	if err != nil || parsed.PageCount < 3 {
		t.Fatalf("ParseGzipBounded: pages=%d error=%v", parsed.PageCount, err)
	}
	if _, err := ParseGzipBounded(bytes.NewReader(data), 64); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("bounded parse error = %v, want ErrInputTooLarge", err)
	}
}

// The fixture is real Tectonic output for testdata/multipage.tex, a three-section
// document that runs to four pages. Regenerate with:
//
//	cd internal/synctex/testdata
//	tectonic -X compile multipage.tex --outdir out --synctex --untrusted
//	cp out/multipage.synctex.gz . && rm -rf out
func load(t *testing.T) *File {
	t.Helper()
	f, err := ParseFile(filepath.Join("testdata", "multipage.synctex.gz"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return f
}

// tagFor finds the input tag for the fixture's own source file.
func tagFor(t *testing.T, f *File) int {
	t.Helper()
	tag, ok := f.TagFor(func(p string) bool { return strings.HasSuffix(p, "multipage.tex") })
	if !ok {
		t.Fatalf("no input tag for multipage.tex; inputs: %v", f.Inputs)
	}
	return tag
}

func TestParsePreamble(t *testing.T) {
	f := load(t)

	if f.Version != "1" {
		t.Errorf("Version = %q, want 1", f.Version)
	}
	if got := len(f.Inputs); got == 0 {
		t.Fatal("no Input records parsed")
	}
	// Engines emit placeholder Input records with no path; those are not
	// something a writer could ever open.
	for tag, path := range f.Inputs {
		if strings.TrimSpace(path) == "" {
			t.Errorf("tag %d has an empty path", tag)
		}
	}
	tagFor(t, f)
}

// The done-when for this phase is a multi-page document, so the fixture must
// actually be one.
func TestParseFindsAllPages(t *testing.T) {
	f := load(t)

	if f.PageCount < 3 {
		t.Fatalf("PageCount = %d; the fixture is supposed to be several pages", f.PageCount)
	}
	for page := 1; page <= f.PageCount; page++ {
		if len(f.Pages[page]) == 0 {
			t.Errorf("page %d has no records", page)
		}
	}
}

// This is the test that pins the unit conversion.
//
// TeX's point is 1/72.27 inch and PDF's is 1/72, so scaled points do not convert
// to PDF points by dividing by 65536 — it is 65536 * 72.27/72. The document has
// the default one-inch margin, which lands on exactly 72 PDF points only with the
// right constant; the wrong one puts it at 72.27 and everything drifts by 0.375%
// thereafter.
func TestCoordinatesAreInPDFPoints(t *testing.T) {
	f := load(t)

	leftmost := math.Inf(1)
	for _, r := range f.Pages[1] {
		if r.X < leftmost {
			leftmost = r.X
		}
	}
	if math.Abs(leftmost-72) > 0.05 {
		t.Errorf("leftmost x on page 1 = %.4f, want 72.0 (the 1in margin). "+
			"72.27 means sp were divided by 65536 instead of %.2f", leftmost, spPerPDFPoint)
	}
}

// Everything must land on the page. A gross unit mistake shows up here as
// coordinates in the thousands.
func TestCoordinatesStayOnThePage(t *testing.T) {
	f := load(t)

	// US Letter, the article class default.
	const width, height = 612.0, 792.0
	for page := 1; page <= f.PageCount; page++ {
		for _, r := range f.Pages[page] {
			if r.X < -1 || r.X > width+1 || r.Y < -1 || r.Y > height+1 {
				t.Fatalf("page %d record at (%.1f, %.1f) is off a %.0fx%.0f page",
					page, r.X, r.Y, width, height)
			}
		}
	}
}

// Y grows downward from the top of the page. If this is inverted, a forward
// search scrolls to the mirror image of the right place — which looks plausible
// enough to ship by accident.
func TestYGrowsDownward(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	// The title (line 8, \maketitle) sits above the first section heading
	// (line 10) on page 1.
	title := f.Forward(tag, 8)
	heading := f.Forward(tag, 10)
	if len(title) == 0 || len(heading) == 0 {
		t.Fatalf("missing rects: title=%v heading=%v", title, heading)
	}
	if title[0].Page != 1 || heading[0].Page != 1 {
		t.Fatalf("expected both on page 1, got %d and %d", title[0].Page, heading[0].Page)
	}
	if !(title[0].Y < heading[0].Y) {
		t.Errorf("title y=%.1f is not above heading y=%.1f; the axis is inverted",
			title[0].Y, heading[0].Y)
	}
}

// Forward search: later sections must land on later pages.
func TestForwardMapsSectionsToIncreasingPages(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	// Lines of the three \section commands in the fixture.
	const first, second, third = 10, 15, 20

	pageOf := func(line int) int {
		rects := f.Forward(tag, line)
		if len(rects) == 0 {
			t.Fatalf("line %d produced no rects", line)
		}
		return rects[0].Page
	}

	p1, p2, p3 := pageOf(first), pageOf(second), pageOf(third)
	if p1 != 1 {
		t.Errorf("first section on page %d, want 1", p1)
	}
	if p1 >= p2 || p2 >= p3 {
		t.Errorf("sections map to pages %d, %d, %d; expected strictly increasing", p1, p2, p3)
	}
}

// A line with no output of its own must still land somewhere: putting the cursor
// on a blank line between paragraphs is completely ordinary.
func TestForwardFallsForwardFromAnEmptyLine(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	// Line 9 is blank, between \maketitle and \section{First section}.
	rects := f.Forward(tag, 9)
	if len(rects) == 0 {
		t.Fatal("a blank line produced nothing; the cursor would jump nowhere")
	}
	if rects[0].Page != 1 {
		t.Errorf("landed on page %d, want 1", rects[0].Page)
	}
}

// Preamble lines produce no output at all, and inventing a location for them
// would be worse than admitting there is none.
func TestForwardOnAnUnknownTagReturnsNothing(t *testing.T) {
	f := load(t)
	if rects := f.Forward(99999, 10); len(rects) != 0 {
		t.Errorf("got %v for a tag that does not exist", rects)
	}
}

// The round trip is the real test of both directions agreeing: forward to a spot
// on the page, click it, and arrive back at the line you started on.
func TestRoundTripForwardThenInverse(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	// Text lines spread across the document, skipping blanks and \lipsum.
	for _, line := range []int{10, 11, 15, 16, 20, 21} {
		rects := f.Forward(tag, line)
		if len(rects) == 0 {
			t.Errorf("line %d: no rects", line)
			continue
		}
		r := rects[0]
		hit, ok := f.Inverse(r.Page, r.X+r.Width/2, r.Y+r.Height/2)
		if !ok {
			t.Errorf("line %d: inverse found nothing on page %d", line, r.Page)
			continue
		}
		if hit.Tag != tag {
			t.Errorf("line %d: inverse gave tag %d, want %d", line, hit.Tag, tag)
		}
		// Exact agreement is not always possible — a click lands inside a box
		// that a neighbouring line also touches — but it must be close.
		if diff := hit.Line - line; diff < -1 || diff > 1 {
			t.Errorf("round trip for line %d came back as %d", line, hit.Line)
		}
	}
}

// Inverse search on an empty page must not fabricate an answer.
func TestInverseOnAPageWithNoRecords(t *testing.T) {
	f := load(t)
	if _, ok := f.Inverse(f.PageCount+50, 300, 400); ok {
		t.Error("inverse invented a hit on a page that does not exist")
	}
}

// Clicking inside a typeset line must land inside a box, not merely near one.
//
// The point comes from a forward search rather than being picked by hand: an
// arbitrary y often falls in the space between two lines, where being "near" is
// the correct answer and asserting otherwise tests nothing.
func TestInverseInsideALineIsAnExactHit(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	rects := f.Forward(tag, 16)
	if len(rects) == 0 {
		t.Fatal("line 16 produced no rects")
	}
	r := rects[0]

	hit, ok := f.Inverse(r.Page, r.X+r.Width/2, r.Y+r.Height/2)
	if !ok {
		t.Fatal("no hit inside a line's own area")
	}
	if hit.Distance != 0 {
		t.Errorf("distance = %.2f; a point inside a line's box should be an exact hit", hit.Distance)
	}
	if hit.Line <= 0 {
		t.Errorf("line = %d", hit.Line)
	}
}

// Clicking in the gap between two typeset lines must land on one of them.
//
// This is the case that plain straight-line distance gets wrong. Every page sits
// inside a vbox spanning the text area, attributed to whichever source line was
// current when it was built. A point in the gap is a few points from the lines
// either side but *inside* that vbox, so nearest-distance answers with the page's
// owner — a line number that is real, plausible, and nowhere near where you
// clicked.
//
// (There is deliberately no test that source lines increase monotonically down a
// page: TeX's page-break machinery attributes boxes to lines out of order around
// a break, so that is not a property of real output.)
func TestInverseInTheGapBetweenLinesPicksANeighbour(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	// The second section's heading and the sentence under it, both on page 2.
	heading := f.Forward(tag, 15)
	sentence := f.Forward(tag, 16)
	if len(heading) == 0 || len(sentence) == 0 {
		t.Fatalf("missing rects: heading=%v sentence=%v", heading, sentence)
	}
	if heading[0].Page != sentence[0].Page {
		t.Skipf("heading and sentence fell on different pages (%d, %d)", heading[0].Page, sentence[0].Page)
	}

	gapTop := heading[0].Y + heading[0].Height
	gapBottom := sentence[0].Y
	if gapBottom <= gapTop {
		t.Skip("no vertical gap between the two lines in this layout")
	}
	midGap := (gapTop + gapBottom) / 2

	hit, ok := f.Inverse(heading[0].Page, heading[0].X+10, midGap)
	if !ok {
		t.Fatal("no hit in the gap")
	}

	// Checked geometrically rather than by line number, because TeX attributes a
	// paragraph to the blank line that *ends* it: the sentence written on line 16
	// is reported as line 17. Demanding an exact line number would be asserting
	// something untrue about TeX. What must hold is that the click resolved to
	// one of the two lines bracketing the gap.
	const lineHeight = 14.0
	if hit.Rect.Y+hit.Rect.Height < gapTop-lineHeight || hit.Rect.Y > gapBottom+lineHeight {
		t.Errorf("clicking the gap at y=%.1f resolved to line %d at y=%.1f..%.1f, "+
			"which is not adjacent to the gap (%.1f..%.1f)",
			midGap, hit.Line, hit.Rect.Y, hit.Rect.Y+hit.Rect.Height, gapTop, gapBottom)
	}
	if hit.Rect.Height > 100 {
		t.Errorf("matched a %.0fx%.0f area — the page box, not a line",
			hit.Rect.Width, hit.Rect.Height)
	}
}

// Whatever a click resolves to must be line-sized. Resolving to the page's own
// vbox is the failure mode above, and it is recognisable by its size.
func TestInverseResolvesToALineNotThePage(t *testing.T) {
	f := load(t)

	for _, y := range []float64{150, 300, 450, 600} {
		hit, ok := f.Inverse(2, 300, y)
		if !ok {
			t.Fatalf("y=%.0f: no hit", y)
		}
		if hit.Rect.Height > 100 {
			t.Errorf("y=%.0f: matched a %.0fx%.0f area for line %d — that is a page box, not a line",
				y, hit.Rect.Width, hit.Rect.Height, hit.Line)
		}
	}
}

// Likewise for forward search: a source line that produces pages of output must
// still highlight where it starts, not the whole page.
func TestForwardHighlightsALineNotThePage(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	// Line 13 is \lipsum[1-6] — six paragraphs from one source line.
	for _, rect := range f.Forward(tag, 13) {
		if rect.Height > 100 {
			t.Errorf("page %d: highlight is %.0fx%.0f — the whole text block, not a line",
				rect.Page, rect.Width, rect.Height)
		}
	}
}

// --- included files ----------------------------------------------------------

// The second fixture is testdata/twofile.tex, which \inputs a chapter that starts
// on page two. Regenerate the same way as multipage.
func loadTwoFile(t *testing.T) *File {
	t.Helper()
	f, err := ParseFile(filepath.Join("testdata", "twofile.synctex.gz"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return f
}

// Input records are not confined to the preamble: the engine emits one wherever
// it first opens a file, so a chapter beginning on page two has its Input record
// buried in the content. Reading only the preamble made every such file invisible
// to both searches — which presented as "SyncTeX just doesn't work for that
// chapter".
func TestInputRecordsInsideContentAreFound(t *testing.T) {
	f := loadTwoFile(t)

	tag, ok := f.TagFor(func(p string) bool { return strings.HasSuffix(p, "chapters/second.tex") })
	if !ok {
		t.Fatalf("the included chapter is missing from Inputs: %v", f.Inputs)
	}

	// And it must be usable, not merely present.
	rects := f.Forward(tag, 3)
	if len(rects) == 0 {
		t.Fatal("no rects for a line in the included chapter")
	}
	if rects[0].Page < 2 {
		t.Errorf("the chapter starts on page %d; the fixture puts it on page 2 or later", rects[0].Page)
	}
}

// A line whose only records are kerns and glue must still resolve to somewhere.
//
// This is extremely common: TeX attributes a paragraph's text to the line that
// *ends* it, leaving the lines that hold the actual words carrying nothing but
// spacing. Falling back to "the nearest line present in the file" lands on
// another such line and reports nothing, making whole paragraphs unreachable.
func TestForwardResolvesLinesCarryingOnlySpacing(t *testing.T) {
	f := loadTwoFile(t)

	tag, ok := f.TagFor(func(p string) bool { return strings.HasSuffix(p, "chapters/second.tex") })
	if !ok {
		t.Fatal("no tag for the included chapter")
	}

	// Find a line that has records but nothing pointable, which is the case the
	// fallback exists for.
	hasAny := map[int]bool{}
	hasTextish := map[int]bool{}
	for _, records := range f.Pages {
		for _, r := range records {
			if r.Tag != tag {
				continue
			}
			hasAny[r.Line] = true
			if isTextish(r.Kind) {
				hasTextish[r.Line] = true
			}
		}
	}

	spacingOnly := 0
	for line := range hasAny {
		if !hasTextish[line] {
			spacingOnly = line
			break
		}
	}
	if spacingOnly == 0 {
		t.Skip("this layout has no spacing-only line to exercise the fallback")
	}

	if rects := f.Forward(tag, spacingOnly); len(rects) == 0 {
		t.Errorf("line %d carries only spacing and resolved to nothing; "+
			"the cursor would have nowhere to go", spacingOnly)
	}
}

// A chapter's first line owns more than the chapter's first line of output.
//
// When TeX reaches \input, it finishes the paragraph already in progress and
// attributes those broken-off lines to the newly opened file's first line. So
// \section{Second} on line 1 of the chapter owns both its heading and the tail of
// the previous chapter's paragraph — nine lines of it, inches higher up the page.
// Pointing at the topmost of those takes the writer to the end of the previous
// chapter instead of the heading they asked for.
func TestForwardPrefersTheLineOwnOutputOverSpillover(t *testing.T) {
	f := loadTwoFile(t)

	tag, ok := f.TagFor(func(p string) bool { return strings.HasSuffix(p, "chapters/second.tex") })
	if !ok {
		t.Fatal("no tag for the included chapter")
	}

	// Every box the chapter's first line owns.
	var tops []float64
	for _, records := range f.Pages {
		for _, r := range records {
			if r.Tag == tag && r.Line == 1 && isTextish(r.Kind) {
				tops = append(tops, r.rect().Y)
			}
		}
	}
	if len(tops) < 3 {
		t.Skipf("this layout gives line 1 only %d boxes; nothing to disambiguate", len(tops))
	}
	highest := math.Inf(1)
	lowest := math.Inf(-1)
	for _, y := range tops {
		highest = math.Min(highest, y)
		lowest = math.Max(lowest, y)
	}

	rects := f.Forward(tag, 1)
	if len(rects) == 0 {
		t.Fatal("line 1 of the chapter resolved to nothing")
	}
	band := rects[0]

	// The heading, not the spillover above it. Compared loosely because the band
	// is a union: a section number sits in its own box a fraction of a point
	// above the title.
	if band.Y-highest < 20 {
		t.Errorf("band at y=%.1f is up with the spillover (topmost box y=%.1f); "+
			"the chapter's own heading is at y=%.1f", band.Y, highest, lowest)
	}
	if lowest < band.Y-1 || lowest > band.Y+band.Height+1 {
		t.Errorf("band %.1f..%.1f does not cover the chapter's own heading at y=%.1f",
			band.Y, band.Y+band.Height, lowest)
	}
	// And it must be one line, not a block swallowing the whole paragraph.
	if band.Height > 20 {
		t.Errorf("band height %.1f spans more than a line of text", band.Height)
	}
}

func TestParseRejectsSomethingElseEntirely(t *testing.T) {
	if _, err := Parse(strings.NewReader("this is not a synctex file\n")); err == nil {
		t.Error("parsed a file with no Content section")
	}
}

func TestParseHandlesRecordsWithAColumn(t *testing.T) {
	// Some producers include a column in the identifier triple.
	in := "SyncTeX Version:1\nInput:1:main.tex\nUnit:1\nX Offset:0\nY Offset:0\nContent:\n{1\n" +
		"(1,42,7:4736287,4736287:1000000,500000,0\n)\n}1\nPost:\n"
	f, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	records := f.Pages[1]
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Line != 42 {
		t.Errorf("Line = %d, want 42 (the column must not be read as the line)", records[0].Line)
	}
}

// --- how much of a line's output the highlight covers -------------------------

// boxesOn returns every piece of material a source line put on one page.
//
// It goes through candidatesOn rather than filtering on isTextish directly, so
// it sees exactly what the highlight code sees — including the fallback for a
// page whose records are all structural. Filtering independently made this
// return nothing and quietly skipped the test it supports.
func boxesOn(f *File, tag, line, page int) []Rect {
	var out []Rect
	for _, r := range f.candidatesOn(page) {
		if r.Tag == tag && r.Line == line {
			out = append(out, r.rect())
		}
	}
	return out
}

// A line that produces a block of prose should light up the whole block, not
// just the row it starts on. Showing only the first row was the old behaviour
// and it made a paragraph look like a one-line entry.
func TestForwardCoversEveryRowOfABlock(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	// Line 13 is \lipsum[1-6]: six paragraphs from one source line. The text is
	// attributed to the line that ends the paragraph, so Forward falls forward
	// onto it — either way, putting the cursor there must light up the block.
	rects := f.Forward(tag, 13)
	if len(rects) < 10 {
		t.Fatalf("six paragraphs produced %d bands; the highlight is not covering the block", len(rects))
	}
	// Still rows, though: one band per line of text, never a single box drawn
	// around everything with the whitespace included.
	for _, rect := range rects {
		if rect.Height > 20 {
			t.Errorf("page %d: band is %.0f tall — that is a block, not a row", rect.Page, rect.Height)
		}
	}
}

// The bands have to span the resolved line's material from its first row to its
// last, with no row left out in between.
func TestForwardCoversTheFullVerticalExtent(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	rects := f.Forward(tag, 13)
	if len(rects) == 0 {
		t.Fatal("line 13 resolved to nothing")
	}
	// Line 13 carries no records of its own — TeX attributes a paragraph to the
	// line that ends it — so Forward falls forward. The bands report which line
	// they actually came from, and that is what their extent must be measured
	// against.
	resolved := rects[0].SourceLine

	// Measure on whichever page holds the most of it.
	counts := map[int]int{}
	for _, rect := range rects {
		counts[rect.Page]++
	}
	page, best := 0, 0
	for candidate, n := range counts {
		if n > best {
			page, best = candidate, n
		}
	}

	boxes := boxesOn(f, tag, resolved, page)
	if len(boxes) < 5 {
		t.Fatalf("line %d has only %d boxes on page %d; the fixture cannot show coverage",
			resolved, len(boxes), page)
	}

	top, bottom := math.Inf(1), math.Inf(-1)
	for _, box := range boxes {
		top = math.Min(top, box.Y)
		bottom = math.Max(bottom, box.Y+box.Height)
	}

	var bands []Rect
	for _, rect := range rects {
		if rect.Page == page {
			bands = append(bands, rect)
		}
	}

	first, last := bands[0], bands[len(bands)-1]
	if first.Y > top+1 {
		t.Errorf("highlight starts at y=%.1f but line %d's output starts at y=%.1f",
			first.Y, resolved, top)
	}
	if last.Y+last.Height < bottom-1 {
		t.Errorf("highlight ends at y=%.1f but line %d's output ends at y=%.1f",
			last.Y+last.Height, resolved, bottom)
	}

	// And nothing skipped in the middle: consecutive bands must not leave a gap
	// wider than the line spacing they sit on.
	for i := 1; i < len(bands); i++ {
		gap := bands[i].Y - (bands[i-1].Y + bands[i-1].Height)
		if gap > bands[i-1].Height*1.5+2 {
			t.Errorf("a %.1fpt gap between bands %d and %d leaves a row unhighlighted", gap, i-1, i)
		}
	}
}

// Bands arrive in reading order and never overlap, so drawing them produces a
// clean run rather than stacked boxes with doubled edges.
func TestForwardBandsAreOrderedAndDisjoint(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	rects := f.Forward(tag, 13)
	for i := 1; i < len(rects); i++ {
		previous, current := rects[i-1], rects[i]
		if current.Page < previous.Page {
			t.Fatalf("band %d is on page %d, after a band on page %d", i, current.Page, previous.Page)
		}
		if current.Page != previous.Page {
			continue
		}
		if current.Y < previous.Y {
			t.Errorf("band %d (y=%.1f) comes before band %d (y=%.1f) on page %d",
				i, current.Y, i-1, previous.Y, current.Page)
		}
		if current.Y < previous.Y+previous.Height-0.5 {
			t.Errorf("bands %d and %d overlap on page %d: %.1f..%.1f then %.1f",
				i-1, i, current.Page, previous.Y, previous.Y+previous.Height, current.Y)
		}
	}
}

// A heading is one row and must stay one band; the change must not turn every
// highlight into a multi-row smear.
func TestForwardOnASingleRowStaysOneBand(t *testing.T) {
	f := load(t)
	tag := tagFor(t, f)

	// Line 10 is \section{First section}.
	if rects := f.Forward(tag, 10); len(rects) != 1 {
		t.Errorf("a one-line heading produced %d bands, want 1", len(rects))
	}
}
