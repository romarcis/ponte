package main

import "testing"

func TestTextOfferKeepsLocalClipboard(t *testing.T) {
	cb := &fakeClip{text: "local"}
	cs := &clipSync{app: &App{cfg: &config{}}, cb: cb, last: "local"}
	cs.receivedIn(&inbox{from: "other"}, wbuf{msgTextOffer}.u32(7))
	if text, _ := cb.Get(); text != "local" || !cs.armed.Load() {
		t.Fatal("text offer changed local clipboard")
	}
	offer, ok := cs.takeOffer(false)
	if !ok || !offer.text || offer.id != 7 {
		t.Fatal("wrong text offer")
	}
	if text, _ := cb.Get(); text != "local" {
		t.Fatal("request changed local clipboard before response")
	}
	if !cs.applyText("remote", offer) {
		t.Fatal("requested text was not accepted")
	}
	if text, _ := cb.Get(); text != "remote" || cs.armed.Load() {
		t.Fatal("requested text not ready for ordinary Paste")
	}
	if cs.offeredText != "" {
		t.Fatal("received text was offered back to other computers")
	}
}

func TestLocalCopyWinsWhileTextIsBeingFetched(t *testing.T) {
	cb := &fakeClip{text: "old local"}
	cs := &clipSync{app: &App{cfg: &config{}}, cb: cb, last: "old local"}
	cs.receivedIn(&inbox{from: "other"}, wbuf{msgTextOffer}.u32(9))
	offer, _ := cs.takeOffer(false)
	cb.Set("new local")
	cs.poll()
	if cs.applyText("remote reply", offer) {
		t.Fatal("remote response replaced new local copy")
	}
	if text, _ := cb.Get(); text != "new local" {
		t.Fatal("local text was lost")
	}
	if cs.armed.Load() {
		t.Fatal("new local copy did not cancel remote offer")
	}
}

func TestTextOfferWithdrawnAndDisconnected(t *testing.T) {
	for _, withdraw := range []bool{true, false} {
		cb := &fakeClip{text: "local"}
		cs := &clipSync{app: &App{cfg: &config{}}, cb: cb, last: "local"}
		in := &inbox{from: "other"}
		cs.receivedIn(in, wbuf{msgTextOffer}.u32(2))
		offer, _ := cs.takeOffer(false)
		if withdraw {
			cs.receivedIn(in, wbuf{msgTextOffer}.u32(0))
		} else {
			cs.peerGone("other")
		}
		if cs.applyText("stale", offer) || cs.armed.Load() {
			t.Fatal("stale text offer survived withdrawal/disconnection")
		}
	}
}

func TestTextPayloadNotBroadcastOnCopy(t *testing.T) {
	cb := &fakeClip{}
	var messages [][]byte
	cs := &clipSync{app: &App{cfg: &config{}}, cb: cb, send: func(b []byte) { messages = append(messages, b) }}
	cb.Set("private text")
	cs.poll()
	if len(messages) != 1 || messages[0][0] != msgTextOffer || len(messages[0]) != 5 {
		t.Fatal("copy sent text instead of just an offer")
	}
	if text, ok := cs.textOfOffer(cs.textID); !ok || text != "private text" {
		t.Fatal("local text not available on request")
	}
	oldID := cs.textID
	cb.Set("new text")
	cs.poll()
	if _, ok := cs.textOfOffer(oldID); ok {
		t.Fatal("old offer served after another local copy")
	}
}

func TestUnrequestedTextCannotOverwriteClipboard(t *testing.T) {
	cb := &fakeClip{text: "local"}
	n := &node{cs: &clipSync{cb: cb}}
	n.textArrived(&link{id: "other"}, 1, "unsolicited")
	if text, _ := cb.Get(); text != "local" {
		t.Fatal("unrequested text overwrote clipboard")
	}
}

func TestDisablingClipboardCancelsPendingText(t *testing.T) {
	cb := &fakeClip{text: "local"}
	cs := &clipSync{app: &App{cfg: &config{}}, cb: cb, last: "local"}
	cs.receivedIn(&inbox{from: "other"}, wbuf{msgTextOffer}.u32(3))
	offer, _ := cs.takeOffer(false)
	cs.resetOffers()
	if cs.applyText("remote", offer) || cs.armed.Load() {
		t.Fatal("disabled sharing left a paste pending")
	}
}

func TestLateUnavailableReplyKeepsNewTextOffer(t *testing.T) {
	cs := &clipSync{app: &App{cfg: &config{}}, cb: &fakeClip{}}
	in := &inbox{from: "other"}
	cs.receivedIn(in, wbuf{msgTextOffer}.u32(1))
	cs.receivedIn(in, wbuf{msgTextOffer}.u32(2))
	cs.unavailable("other", 1)
	offer, ok := cs.takeOffer(false)
	if !ok || offer.id != 2 {
		t.Fatal("old failed request canceled a newer copy")
	}
}

func TestReturningToSourceFetchesTextCopiedOnDestination(t *testing.T) {
	pcs := newTestPCs(t, "Source", "Destination")
	a, b := pcs[0], pcs[1]
	b.app.connect(a.id(), a.addr(), a.app.status().Code)
	waitFor(t, "linked", func() bool { return a.online(b) && b.online(a) })
	a.clip.Set("copied on A")
	waitFor(t, "offer on B", func() bool { return b.app.node.cs.armed.Load() })
	if text, _ := a.clip.Get(); text != "copied on A" {
		t.Fatal("could not paste locally on source")
	}
	a.cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "text prepared on B", func() bool { text, _ := b.clip.Get(); return text == "copied on A" })
	if b.inj.has("key 47 1") {
		t.Fatal("screen entry unexpectedly pasted into an application")
	}
	b.clip.Set("copied on B")
	waitFor(t, "offer on A", func() bool { return a.app.node.cs.armed.Load() })
	if text, _ := a.clip.Get(); text != "copied on A" {
		t.Fatal("copy on B changed A before return")
	}
	a.cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "text prepared on return to A", func() bool { text, _ := a.clip.Get(); return text == "copied on B" })
	if text, _ := b.clip.Get(); text != "copied on B" {
		t.Fatal("fetch changed source clipboard on B")
	}
}

func TestLayoutRequiresSideConnections(t *testing.T) {
	l := layout{Pos: map[string]cell{"a": {0, 0}, "b": {1, 0}, "c": {2, 0}}}
	if l.canMove("b", cell{1, 1}) {
		t.Fatal("moving bridge disconnected the two ends")
	}
	if l.canMove("c", cell{3, 0}) || l.canMove("c", cell{2, 1}) {
		t.Fatal("isolated/diagonal placement accepted")
	}
	if !l.canMove("c", cell{1, 1}) {
		t.Fatal("valid side-adjacent placement rejected")
	}
	if _, ok := l.neighbor("a", "invalid"); ok {
		t.Fatal("invalid direction accepted")
	}
}

func TestActiveLayoutClosesGapAndRestoresMiddle(t *testing.T) {
	for _, step := range []cell{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
		l := layout{Pos: map[string]cell{"a": {0, 0}, "b": step, "c": {2 * step.X, 2 * step.Y}}}
		active := l.active("a", map[string]bool{"c": true})
		if len(active.Pos) != 2 || !active.connected() {
			t.Fatal("offline middle left a gap")
		}
		distance := abs(active.Pos["a"].X-active.Pos["c"].X) + abs(active.Pos["a"].Y-active.Pos["c"].Y)
		if distance != 1 {
			t.Fatal("remaining screens are not adjacent")
		}
		if len(l.Pos) != 3 || l.Pos["b"] != step {
			t.Fatal("saved arrangement changed on disconnection")
		}
		restored := l.active("a", map[string]bool{"b": true, "c": true})
		if !sameLayout(restored, l) {
			t.Fatal("reconnection did not restore original map")
		}
		other := l.active("c", map[string]bool{"a": true})
		if !sameLayout(other, active) {
			t.Fatal("two online computers computed different active maps")
		}
	}
}

func TestOfflineMiddleScreenStillAllowsMouseSwitch(t *testing.T) {
	pcs := newTestPCs(t, "Left", "Middle", "Right")
	a, b, c := pcs[0], pcs[1], pcs[2]
	b.app.connect(a.id(), a.addr(), a.app.status().Code)
	waitFor(t, "a and b linked", func() bool { return a.online(b) && b.online(a) })
	c.app.connect(a.id(), a.addr(), a.app.status().Code)
	waitFor(t, "whole group linked", func() bool { return a.online(c) && b.online(c) && c.online(b) })
	if !a.app.moveScreen(c.id(), cell{2, 0}) {
		t.Fatal("could not arrange a row")
	}
	waitFor(t, "row shared", func() bool { return c.pos(c) == (cell{2, 0}) && b.pos(c) == (cell{2, 0}) })
	b.app.shutdown()
	waitFor(t, "middle offline", func() bool { return !a.online(b) && !c.online(b) })
	if _, exists := a.app.status().Layout[b.id()]; exists {
		t.Fatal("offline screen still blocks the active map")
	}
	a.cap.ch <- inputEvent{kind: evMotion}
	a.cap.ch <- inputEvent{kind: evPos, x: 999, y: 400}
	waitFor(t, "left reaches right across offline middle", func() bool { return c.app.status().By == a.id() })
	a.cap.ch <- inputEvent{kind: evRel, x: -100, y: 0}
	waitFor(t, "right returns to left", func() bool { return !a.cap.grabbed() })
	makeCapture = func() inputCapture { return b.cap }
	makeInjector = func() inputInjector { return b.inj }
	makeClipboard = func() clipboard { return b.clip }
	if err := b.app.start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "middle reconnects", func() bool { return a.online(b) && c.online(b) })
	if a.app.status().Layout[c.id()] != (cell{2, 0}) {
		t.Fatal("original row did not return")
	}
}
