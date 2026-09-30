package main

import (
	"sort"
)

// The screen map: every computer of the group sits on a cell of a grid, and
// the pointer passes from a screen to the one in the next cell. All the
// computers share the same map; the most recent change wins.

type cell struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type layout struct {
	Stamp int64           `json:"stamp"` // when it was last changed (unix nanoseconds)
	Pos   map[string]cell `json:"pos"`   // device ID (hex) -> cell
}

// Directions, in the order the screen edges are checked.
var dirs = map[string]cell{"right": {1, 0}, "left": {-1, 0}, "bottom": {0, 1}, "top": {0, -1}}

var opposite = map[string]string{"right": "left", "left": "right", "top": "bottom", "bottom": "top"}

func (l *layout) clone() layout {
	c := layout{Stamp: l.Stamp, Pos: make(map[string]cell, len(l.Pos))}
	for k, v := range l.Pos {
		c.Pos[k] = v
	}
	return c
}

// at returns who sits on c.
func (l *layout) at(c cell) (string, bool) {
	for id, p := range l.Pos {
		if p == c {
			return id, true
		}
	}
	return "", false
}

// neighbor returns who sits next to id in direction dir.
func (l layout) neighbor(id, dir string) (string, bool) {
	p, ok := l.Pos[id]
	if !ok {
		return "", false
	}
	d, valid := dirs[dir]
	if !valid {
		return "", false
	}
	return l.at(cell{p.X + d.X, p.Y + d.Y})
}

// connected rejects isolated/diagonal screens and overlapping cells.
func (l *layout) connected() bool {
	if len(l.Pos) == 0 {
		return true
	}
	occupied := map[cell]bool{}
	for _, p := range l.Pos {
		occupied[p] = true
	}
	if len(occupied) != len(l.Pos) {
		return false
	}
	seen := map[cell]bool{}
	queue := []cell{l.Pos[l.order()[0]]}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		for _, d := range dirs {
			q := cell{p.X + d.X, p.Y + d.Y}
			if occupied[q] && !seen[q] {
				queue = append(queue, q)
			}
		}
	}
	return len(seen) == len(occupied)
}

func (l *layout) canMove(id string, to cell) bool {
	if _, ok := l.Pos[id]; !ok {
		return false
	}
	moved := l.clone()
	moved.move(id, to)
	return moved.connected()
}

// active compacts only the online screens. The saved map stays intact, so
// reconnecting a middle computer restores its place in the original row.
func (l *layout) active(self string, online map[string]bool) layout {
	live := layout{Stamp: l.Stamp, Pos: map[string]cell{}}
	for id, p := range l.Pos {
		if id == self || online[id] {
			live.Pos[id] = p
		}
	}
	if live.connected() {
		return live
	}
	ids := live.order()
	if len(ids) == 0 {
		return live
	}
	result := layout{Stamp: l.Stamp, Pos: map[string]cell{ids[0]: live.Pos[ids[0]]}}
	remaining := append([]string{}, ids[1:]...)
	for len(remaining) > 0 {
		// Keep existing adjacent screens in their exact places first.
		index := -1
		for i, id := range remaining {
			p := live.Pos[id]
			if _, used := result.at(p); used {
				continue
			}
			for _, d := range dirs {
				if _, ok := result.at(cell{p.X + d.X, p.Y + d.Y}); ok {
					index = i
					break
				}
			}
			if index >= 0 {
				break
			}
		}
		if index >= 0 {
			id := remaining[index]
			result.Pos[id] = live.Pos[id]
			remaining = append(remaining[:index], remaining[index+1:]...)
			continue
		}
		id := remaining[0]
		wanted := live.Pos[id]
		best := cell{}
		distance := int(^uint(0) >> 1)
		for _, p := range result.Pos {
			for _, d := range dirs {
				candidate := cell{p.X + d.X, p.Y + d.Y}
				if _, used := result.at(candidate); used {
					continue
				}
				delta := abs(candidate.X-wanted.X) + abs(candidate.Y-wanted.Y)
				if delta < distance || (delta == distance && (candidate.Y < best.Y || (candidate.Y == best.Y && candidate.X < best.X))) {
					best, distance = candidate, delta
				}
			}
		}
		result.Pos[id] = best
		remaining = remaining[1:]
	}
	return result
}

// dirTo tells in which direction to is right next to from.
func (l *layout) dirTo(from, to string) (string, bool) {
	for dir := range dirs {
		if n, ok := l.neighbor(from, dir); ok && n == to {
			return dir, true
		}
	}
	return "", false
}

// place puts id on the first free cell next to near: on its right if free,
// then left, below, above, then further along the row. It changes nothing
// if id is already on the map.
func (l *layout) place(id, near string) bool {
	if _, ok := l.Pos[id]; ok {
		return false
	}
	if l.Pos == nil {
		l.Pos = map[string]cell{}
	}
	c, ok := l.Pos[near]
	if !ok {
		c = cell{}
		if _, used := l.at(c); !used {
			l.Pos[id] = c
			return true
		}
	}
	for step := 1; ; step++ {
		for _, d := range []cell{{step, 0}, {-step, 0}, {0, step}, {0, -step}} {
			n := cell{c.X + d.X, c.Y + d.Y}
			if _, used := l.at(n); !used {
				l.Pos[id] = n
				return true
			}
		}
	}
}

// move puts id on c; whoever was there takes id's old cell.
func (l *layout) move(id string, c cell) {
	old, ok := l.Pos[id]
	if other, used := l.at(c); used && other != id {
		if ok {
			l.Pos[other] = old
		} else {
			delete(l.Pos, other)
		}
	}
	l.Pos[id] = c
}

// keep drops the computers that are not in ids and places the missing ones,
// in a stable order so every computer comes to the same map.
func (l *layout) keep(ids []string, near string) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for id := range l.Pos {
		if !want[id] {
			delete(l.Pos, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		l.place(id, near)
	}
}

// order lists the computers row by row, left to right: the order Scroll Lock
// goes through them.
func (l *layout) order() []string {
	ids := make([]string, 0, len(l.Pos))
	for id := range l.Pos {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := l.Pos[ids[i]], l.Pos[ids[j]]
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		if a.X != b.X {
			return a.X < b.X
		}
		return ids[i] < ids[j]
	})
	return ids
}

func encLayout(l layout) []byte {
	b := wbuf{msgLayout}.i64(l.Stamp).u16(uint16(len(l.Pos)))
	for _, id := range sortedKeys(l.Pos) {
		p := l.Pos[id]
		b = b.str(id).i32(int32(p.X)).i32(int32(p.Y))
	}
	return b
}

func decLayout(msg []byte) (layout, bool) {
	r := &rbuf{b: msg[1:]}
	l := layout{Stamp: r.i64(), Pos: map[string]cell{}}
	n := int(r.u16())
	for range n {
		id := r.str()
		l.Pos[id] = cell{int(r.i32()), int(r.i32())}
	}
	return l, r.err == nil && n <= 64
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
