package main

// Кэш объектов на стороне сервера: прочитанные полигоны и список областей,
// которые уже загружены. Пока экран внутри покрытой области, ClickHouse не
// беспокоим. Кэш общий для всех пользователей — данные одни и те же, а
// нагрузка на базу от этого только падает.

import "strings"

type rect struct{ W, S, E, N float64 }

func (r rect) contains(o rect) bool { return o.W >= r.W && o.E <= r.E && o.S >= r.S && o.N <= r.N }
func (r rect) intersects(o rect) bool {
	return r.W <= o.E && r.E >= o.W && r.S <= o.N && r.N >= o.S
}

type cachedObj struct {
	bb  rect
	row map[string]any
}

// Покрытая область помнит, с какой детализацией её грузили: minDeg —
// минимальный размер объекта (в градусах), попадавший тогда в выборку. Для
// более детального запроса такая область покрытием не считается.
type coveredRect struct {
	rect
	minDeg float64
}

type layerCache struct {
	objs  map[string]cachedObj
	rects []coveredRect
}

// geomBBox считает охват мультиполигона из координат [[[[x,y],...]]].
func geomBBox(g any) (rect, bool) {
	bb := rect{W: 1e18, S: 1e18, E: -1e18, N: -1e18}
	found := false
	var walk func(v any)
	walk = func(v any) {
		arr, ok := v.([]any)
		if !ok {
			return
		}
		if len(arr) == 2 { // точка: [x, y]
			x, xo := arr[0].(float64)
			y, yo := arr[1].(float64)
			if xo && yo {
				if x < bb.W {
					bb.W = x
				}
				if x > bb.E {
					bb.E = x
				}
				if y < bb.S {
					bb.S = y
				}
				if y > bb.N {
					bb.N = y
				}
				found = true
				return
			}
		}
		for _, e := range arr {
			walk(e)
		}
	}
	walk(g)
	return bb, found
}

func sqlIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "") + "`" }

// trimSQL — первые n символов одной строкой, для журнала.
func trimSQL(sql string, n int) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
