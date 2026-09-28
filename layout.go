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
func (l *layout) neighbor(id, dir string) (string, bool) {
	p, ok := l.Pos[id]
	if !ok {
		return "", false
	}
	d := dirs[dir]
	return l.at(cell{p.X + d.X, p.Y + d.Y})
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
