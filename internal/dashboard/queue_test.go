package dashboard

import (
	"bytes"
	"strings"
	"testing"
)

// renderPanel renders the page the dashboard actually serves.
//
// RenderControlPage, not Handler: Handler and the template in dashboard.go
// are reachable only from a test today, and a test that rendered them would
// pass while the served page said something else. That is worth stating
// rather than discovering.
func renderPanel(t *testing.T, view ControlView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := RenderControlPage(&buf, view); err != nil {
		t.Fatalf("RenderControlPage: %v", err)
	}
	return buf.String()
}

// TestPanelShowsTheQueueWhenRounceIsPresent is the read side of the join on
// the panel: this is where a person sits to watch agents work, and "what
// should happen next" is the one question the rest of the page cannot
// answer.
func TestPanelShowsTheQueueWhenRounceIsPresent(t *testing.T) {
	view := ControlView{View: View{
		HasRounce: true,
		Queue: []QueueView{
			{ID: "rnc-aaa111", Score: 25, Type: "task",
				Title: "Touch something else", Paths: "cmd/other.go"},
		},
	}}

	page := renderPanel(t, view)
	for _, want := range []string{"rnc-aaa111", "25", "Touch something else", "cmd/other.go"} {
		if !strings.Contains(page, want) {
			t.Errorf("the panel does not show %q:\n%s", want, page)
		}
	}
}

// TestPanelSaysNothingAboutTheQueueWithoutRounce. Interlock keeps agents
// apart in repositories that have never heard of a work store, which is most
// of them. An empty "Next" heading there would imply there is nothing to do,
// when the truth is that nobody asked.
func TestPanelSaysNothingAboutTheQueueWithoutRounce(t *testing.T) {
	page := renderPanel(t, ControlView{View: View{HasRounce: false}})

	if strings.Contains(page, "from rounce") {
		t.Errorf("the panel mentions rounce with no rounce present:\n%s", page)
	}
}

// TestPanelDistinguishesAnEmptyQueueFromABlockedOne. rounce hands over a
// queue it has already filtered, so an empty one has two very different
// causes -- no work left, or all remaining work is held by an agent on this
// same page. Showing nothing would merge them.
func TestPanelDistinguishesAnEmptyQueueFromABlockedOne(t *testing.T) {
	page := renderPanel(t, ControlView{View: View{HasRounce: true, Queue: nil}})

	if !strings.Contains(page, "Nothing is workable") {
		t.Errorf("an empty queue renders no explanation:\n%s", page)
	}
	if !strings.Contains(page, "rounce blocked") {
		t.Errorf("the empty-queue text does not say how to tell the two causes apart:\n%s", page)
	}
}

// TestPanelEscapesQueueText. Titles and paths come from a work store any
// agent can write to, so they reach this page as untrusted text. The
// template escapes them; this is the assertion that it still does.
func TestPanelEscapesQueueText(t *testing.T) {
	view := ControlView{View: View{
		HasRounce: true,
		Queue: []QueueView{{
			ID: "rnc-bbb222", Score: 1, Type: "task",
			Title: `<script>alert(1)</script>`,
			Paths: `"><img src=x onerror=alert(1)>`,
		}},
	}}

	page := renderPanel(t, view)
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Errorf("a queue title rendered as live markup:\n%s", page)
	}
	if strings.Contains(page, "<img src=x") {
		t.Errorf("a queue path rendered as live markup:\n%s", page)
	}
	// Present as inert text, so the escaping is doing the work rather than
	// the field being dropped -- a dropped field would pass vacuously.
	if !strings.Contains(page, "alert(1)") {
		t.Errorf("the hostile text is absent entirely; this test would pass vacuously:\n%s", page)
	}
}
