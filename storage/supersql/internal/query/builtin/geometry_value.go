package builtin

import (
	"context"
	"encoding/binary"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type geoPoint struct{ x, y float64 }
type geoBox struct{ high, low geoPoint }
type geoSegment struct{ p0, p1 geoPoint }
type geoLine struct{ a, b, c float64 }
type geoCircle struct {
	center geoPoint
	radius float64
}
type geoPoints []byte
type geoPath struct {
	points geoPoints
	closed bool
}
type geoPolygon struct{ points geoPoints }

func (points geoPoints) count() int            { return len(points) / 16 }
func (points geoPoints) at(index int) geoPoint { return geoReadPoint(points[index*16:]) }

func geoReadFloat(data []byte) float64 { return math.Float64frombits(binary.BigEndian.Uint64(data)) }
func geoWriteFloat(data []byte, value float64) {
	binary.BigEndian.PutUint64(data, math.Float64bits(value))
}
func geoReadPoint(data []byte) geoPoint { return geoPoint{geoReadFloat(data), geoReadFloat(data[8:])} }
func geoWritePoint(data []byte, point geoPoint) {
	geoWriteFloat(data, point.x)
	geoWriteFloat(data[8:], point.y)
}

// ValidateGeometryValue borrows a compact geometric payload. Fixed fields use
// big-endian float64 bits; paths have a closed byte and uint32 point count,
// polygons only a uint32 point count. Both variable forms end in packed points.
// Arithmetic can produce negative circle radii and inverted box bounds; those
// are valid internal values even though textual constructors normalize inputs.
func ValidateGeometryValue(ctx context.Context, id model.TypeID, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	width := 0
	switch id {
	case 600:
		width = 16
	case 601, 603:
		width = 32
	case 628, 718:
		width = 24
	case 602:
		if len(data) < 5 || data[0] > 1 {
			return invalidGeometry()
		}
		return validateGeoPoints(data[1:])
	case 604:
		return validateGeoPoints(data)
	default:
		return invalidGeometry()
	}
	if len(data) != width {
		return invalidGeometry()
	}
	if id == 628 && geoReadFloat(data) == 0 && geoReadFloat(data[8:]) == 0 {
		return invalidGeometry()
	}
	return nil
}

func validateGeoPoints(data []byte) error {
	if len(data) < 4 {
		return invalidGeometry()
	}
	count := uint64(binary.BigEndian.Uint32(data))
	if count == 0 || count > math.MaxInt32 || count*16 != uint64(len(data)-4) {
		return invalidGeometry()
	}
	return nil
}

func invalidGeometry() error { return sqlError("22P03", "invalid binary geometric value") }

func geoDecodePoint(value model.Value) (geoPoint, error) {
	if value.Type != model.TypeBytes || len(value.BytesVal) != 16 {
		return geoPoint{}, invalidGeometry()
	}
	return geoReadPoint(value.BytesVal), nil
}
func geoDecodeBox(value model.Value) (geoBox, error) {
	if value.Type != model.TypeBytes || len(value.BytesVal) != 32 {
		return geoBox{}, invalidGeometry()
	}
	return geoBox{geoReadPoint(value.BytesVal), geoReadPoint(value.BytesVal[16:])}, nil
}
func geoDecodeSegment(value model.Value) (geoSegment, error) {
	if value.Type != model.TypeBytes || len(value.BytesVal) != 32 {
		return geoSegment{}, invalidGeometry()
	}
	return geoSegment{geoReadPoint(value.BytesVal), geoReadPoint(value.BytesVal[16:])}, nil
}
func geoDecodeLine(value model.Value) (geoLine, error) {
	if value.Type != model.TypeBytes || len(value.BytesVal) != 24 {
		return geoLine{}, invalidGeometry()
	}
	return geoLine{geoReadFloat(value.BytesVal), geoReadFloat(value.BytesVal[8:]), geoReadFloat(value.BytesVal[16:])}, nil
}
func geoDecodeCircle(value model.Value) (geoCircle, error) {
	if value.Type != model.TypeBytes || len(value.BytesVal) != 24 {
		return geoCircle{}, invalidGeometry()
	}
	return geoCircle{geoReadPoint(value.BytesVal), geoReadFloat(value.BytesVal[16:])}, nil
}
func geoDecodePath(value model.Value) (geoPath, error) {
	if value.Type != model.TypeBytes || len(value.BytesVal) < 5 || value.BytesVal[0] > 1 {
		return geoPath{}, invalidGeometry()
	}
	if err := validateGeoPoints(value.BytesVal[1:]); err != nil {
		return geoPath{}, err
	}
	return geoPath{geoPoints(value.BytesVal[5:]), value.BytesVal[0] != 0}, nil
}
func geoDecodePolygon(value model.Value) (geoPolygon, error) {
	if value.Type != model.TypeBytes {
		return geoPolygon{}, invalidGeometry()
	}
	if err := validateGeoPoints(value.BytesVal); err != nil {
		return geoPolygon{}, err
	}
	return geoPolygon{geoPoints(value.BytesVal[4:])}, nil
}

func newGeoPath(ctx context.Context, count int, closed bool, budget CallBudget) (byteResultBuffer, error) {
	if count <= 0 || uint64(count) > math.MaxInt32 {
		return byteResultBuffer{}, sqlError("54000", "invalid geometric point count")
	}
	buffer, err := newByteResult(ctx, 5+uint64(count)*16, budget)
	if err != nil {
		return byteResultBuffer{}, err
	}
	if closed {
		buffer.data[0] = 1
	}
	binary.BigEndian.PutUint32(buffer.data[1:], uint32(count))
	return buffer, nil
}
func newGeoPolygon(ctx context.Context, count int, budget CallBudget) (byteResultBuffer, error) {
	if count <= 0 || uint64(count) > math.MaxInt32 {
		return byteResultBuffer{}, sqlError("54000", "invalid geometric point count")
	}
	buffer, err := newByteResult(ctx, 4+uint64(count)*16, budget)
	if err != nil {
		return byteResultBuffer{}, err
	}
	binary.BigEndian.PutUint32(buffer.data, uint32(count))
	return buffer, nil
}
func geoPointValue(ctx context.Context, point geoPoint, budget CallBudget) (model.Value, error) {
	buffer, err := newByteResult(ctx, 16, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	geoWritePoint(buffer.data, point)
	return buffer.value(), nil
}
func geoBoxValue(ctx context.Context, box geoBox, budget CallBudget) (model.Value, error) {
	buffer, err := newByteResult(ctx, 32, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	geoWritePoint(buffer.data, box.high)
	geoWritePoint(buffer.data[16:], box.low)
	return buffer.value(), nil
}
func geoSegmentValue(ctx context.Context, segment geoSegment, budget CallBudget) (model.Value, error) {
	buffer, err := newByteResult(ctx, 32, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	geoWritePoint(buffer.data, segment.p0)
	geoWritePoint(buffer.data[16:], segment.p1)
	return buffer.value(), nil
}
func geoLineValue(ctx context.Context, line geoLine, budget CallBudget) (model.Value, error) {
	buffer, err := newByteResult(ctx, 24, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	geoWriteFloat(buffer.data, line.a)
	geoWriteFloat(buffer.data[8:], line.b)
	geoWriteFloat(buffer.data[16:], line.c)
	return buffer.value(), nil
}
func geoCircleValue(ctx context.Context, circle geoCircle, budget CallBudget) (model.Value, error) {
	buffer, err := newByteResult(ctx, 24, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	geoWritePoint(buffer.data, circle.center)
	geoWriteFloat(buffer.data[16:], circle.radius)
	return buffer.value(), nil
}
