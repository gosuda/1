package builtin

import "context"

// geoPolygonCrossing preserves PostgreSQL's signed crossing count; 4 denotes
// a point on the boundary and is never a normal crossing count.
func geoPolygonCrossing(x, y, px, py float64) (int, error) {
	if geoFPZero(y) {
		if geoFPZero(x) {
			return 4, nil
		}
		if geoFPGt(x, 0) {
			if geoFPZero(py) {
				if geoFPGt(px, 0) {
					return 0, nil
				}
				return 4, nil
			}
			if geoFPLt(py, 0) {
				return 1, nil
			}
			return -1, nil
		}
		if geoFPZero(py) {
			if geoFPLt(px, 0) {
				return 0, nil
			}
			return 4, nil
		}
		return 0, nil
	}
	sign := -1
	if geoFPGt(y, 0) {
		sign = 1
	}
	if geoFPZero(py) {
		if geoFPLt(px, 0) {
			return 0, nil
		}
		return sign, nil
	}
	sameSide := sign < 0 && geoFPLt(py, 0)
	sameSide = sameSide || (sign > 0 && geoFPGt(py, 0))
	if sameSide {
		return 0, nil
	}
	if geoFPGe(x, 0) && geoFPGt(px, 0) {
		return 2 * sign, nil
	}
	if geoFPLt(x, 0) && geoFPLe(px, 0) {
		return 0, nil
	}
	dx, err := geoSub(x, px)
	if err != nil {
		return 0, err
	}
	a, err := geoMul(dx, y)
	if err != nil {
		return 0, err
	}
	dy, err := geoSub(y, py)
	if err != nil {
		return 0, err
	}
	b, err := geoMul(dy, x)
	if err != nil {
		return 0, err
	}
	z, err := geoSub(a, b)
	if err != nil {
		return 0, err
	}
	if geoFPZero(z) {
		return 4, nil
	}
	outside := sign < 0 && geoFPLt(z, 0)
	outside = outside || (sign > 0 && geoFPGt(z, 0))
	if outside {
		return 0, nil
	}
	return 2 * sign, nil
}

func geoPointInside(ctx context.Context, p geoPoint, points geoPoints) (bool, error) {
	first, err := geoPointSub(points.at(0), p)
	if err != nil {
		return false, err
	}
	previous := first
	total := 0
	for i := 1; i <= points.count(); i++ {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		next := first
		if i < points.count() {
			next, err = geoPointSub(points.at(i), p)
			if err != nil {
				return false, err
			}
		}
		cross, err := geoPolygonCrossing(next.x, next.y, previous.x, previous.y)
		if err != nil {
			return false, err
		}
		if cross == 4 {
			return true, nil
		}
		total += cross
		previous = next
	}
	return total != 0, ctx.Err()
}

func geoPointPolygonDistance(ctx context.Context, p geoPoint, polygon geoPolygon) (float64, error) {
	inside, err := geoPointInside(ctx, p, polygon.points)
	if err != nil || inside {
		return 0, err
	}
	points := polygon.points
	_, result, err := geoSegmentClosestPoint(geoSegment{points.at(0), points.at(points.count() - 1)}, p)
	if err != nil {
		return 0, err
	}
	for i := range points.count() - 1 {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		_, distance, err := geoSegmentClosestPoint(geoSegment{points.at(i), points.at(i + 1)}, p)
		if err != nil {
			return 0, err
		}
		if compareFloat(distance, result) < 0 {
			result = distance
		}
	}
	return result, nil
}

func geoPolygonSame(ctx context.Context, a, b geoPolygon) (bool, error) {
	n := a.points.count()
	if n != b.points.count() {
		return false, nil
	}
	for i := range n {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !geoPointSame(a.points.at(0), b.points.at(i)) {
			continue
		}
		for _, step := range [2]int{1, -1} {
			match, err := geoPolygonSequenceSame(ctx, a.points, b.points, i, step)
			if err != nil || match {
				return match, err
			}
		}
	}
	return false, nil
}

func geoPolygonSequenceSame(ctx context.Context, a, b geoPoints, start, step int) (bool, error) {
	n := a.count()
	for j := 1; j < n; j++ {
		if j%256 == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if !geoPointSame(a.at(j), b.at((start+step*j+n)%n)) {
			return false, nil
		}
	}
	return true, nil
}

func geoPolygonOverlap(ctx context.Context, a, b geoPolygon, ab, bb geoBox) (bool, error) {
	if !geoBoxesOverlap(ab, bb) {
		return false, nil
	}
	intersects, err := geoPathsIntersect(ctx, geoPath{a.points, true}, geoPath{b.points, true})
	if err != nil || intersects {
		return intersects, err
	}
	inside, err := geoPointInside(ctx, a.points.at(0), b.points)
	if err != nil || inside {
		return inside, err
	}
	return geoPointInside(ctx, b.points.at(0), a.points)
}

// PostgreSQL recursively splits crossing segments. Bound both recursion and
// total edge visits so degenerate polygons cannot exhaust stack or CPU.
const geoPolygonContainDepth = 256
const geoPolygonContainWork = 1 << 24

type geoPolygonContainState struct{ remaining int }

func (state *geoPolygonContainState) visit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state.remaining--
	if state.remaining < 0 {
		return sqlError("54000", "polygon containment work limit exceeded")
	}
	return nil
}

func geoPolygonContains(ctx context.Context, a, b geoPolygon, ab, bb geoBox) (bool, error) {
	if !(geoFPGe(ab.high.x, bb.high.x) && geoFPLe(ab.low.x, bb.low.x) && geoFPGe(ab.high.y, bb.high.y) && geoFPLe(ab.low.y, bb.low.y)) {
		return false, nil
	}
	state := geoPolygonContainState{remaining: geoPolygonContainWork}
	previous := b.points.at(b.points.count() - 1)
	for i := range b.points.count() {
		next := b.points.at(i)
		inside, err := state.segmentInside(ctx, previous, next, a, 0, 0)
		if err != nil || !inside {
			return false, err
		}
		previous = next
	}
	return true, nil
}

func (state *geoPolygonContainState) touchedInside(ctx context.Context, a, b geoPoint, s geoSegment, polygon geoPolygon, start, depth int) (bool, error) {
	t := geoSegment{a, b}
	if geoPointSame(a, s.p0) {
		contains, err := geoSegmentContainsPoint(t, s.p1)
		if err != nil {
			return false, err
		}
		if contains {
			return state.segmentInside(ctx, b, s.p1, polygon, start, depth+1)
		}
	} else if geoPointSame(a, s.p1) {
		contains, err := geoSegmentContainsPoint(t, s.p0)
		if err != nil {
			return false, err
		}
		if contains {
			return state.segmentInside(ctx, b, s.p0, polygon, start, depth+1)
		}
	} else {
		contains, err := geoSegmentContainsPoint(t, s.p0)
		if err != nil {
			return false, err
		}
		if contains {
			return state.segmentInside(ctx, b, s.p0, polygon, start, depth+1)
		}
		contains, err = geoSegmentContainsPoint(t, s.p1)
		if err != nil {
			return false, err
		}
		if contains {
			return state.segmentInside(ctx, b, s.p1, polygon, start, depth+1)
		}
	}
	return true, nil
}

type geoContainDecision uint8

const (
	geoContainScan geoContainDecision = iota
	geoContainAccept
	geoContainReject
)

func (state *geoPolygonContainState) edgeInside(ctx context.Context, query, edge geoSegment, polygon geoPolygon, start, depth int) (geoContainDecision, bool, error) {
	a, b := query.p0, query.p1
	onA, err := geoSegmentContainsPoint(edge, a)
	if err != nil {
		return geoContainReject, false, err
	}
	onB, err := geoSegmentContainsPoint(edge, b)
	if err != nil {
		return geoContainReject, false, err
	}
	if onA && onB {
		return geoContainAccept, false, nil
	}
	if onA || onB {
		if onB {
			a, b = b, a
		}
		inside, err := state.touchedInside(ctx, a, b, edge, polygon, start, depth)
		if err != nil || !inside {
			return geoContainReject, false, err
		}
		return geoContainScan, false, nil
	}
	point, crosses, err := geoSegmentIntersection(query, edge)
	if err != nil || !crosses {
		return geoContainScan, false, err
	}
	inside, err := state.segmentInside(ctx, a, point, polygon, start, depth+1)
	if err != nil || !inside {
		return geoContainReject, true, err
	}
	inside, err = state.segmentInside(ctx, b, point, polygon, start, depth+1)
	if err != nil || !inside {
		return geoContainReject, true, err
	}
	return geoContainScan, true, nil
}

func (state *geoPolygonContainState) segmentInside(ctx context.Context, a, b geoPoint, polygon geoPolygon, start, depth int) (bool, error) {
	if depth >= geoPolygonContainDepth {
		return false, sqlError("54001", "stack depth limit exceeded")
	}
	if err := state.visit(ctx); err != nil {
		return false, err
	}
	t := geoSegment{a, b}
	previous := polygon.points.count() - 1
	if start != 0 {
		previous = start - 1
	}
	s := geoSegment{p0: polygon.points.at(previous)}
	intersection := false
	for i := start; i < polygon.points.count(); i++ {
		if err := state.visit(ctx); err != nil {
			return false, err
		}
		s.p1 = polygon.points.at(i)
		decision, crossed, err := state.edgeInside(ctx, t, s, polygon, i+1, depth)
		if err != nil {
			return false, err
		}
		if decision != geoContainScan {
			return decision == geoContainAccept, nil
		}
		intersection = intersection || crossed
		s.p0 = s.p1
	}
	if intersection {
		return true, nil
	}
	x, err := geoAdd(a.x, b.x)
	if err != nil {
		return false, err
	}
	x, err = geoDiv(x, 2)
	if err != nil {
		return false, err
	}
	y, err := geoAdd(a.y, b.y)
	if err != nil {
		return false, err
	}
	y, err = geoDiv(y, 2)
	if err != nil {
		return false, err
	}
	// Charge the full winding scan against the same operation budget.
	state.remaining -= polygon.points.count()
	if state.remaining < 0 {
		return false, sqlError("54000", "polygon containment work limit exceeded")
	}
	return geoPointInside(ctx, geoPoint{x, y}, polygon.points)
}
