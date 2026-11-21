package wire

import (
	"testing"
)

func BenchmarkFrameBuilderStream(b *testing.B) {
	buf := make([]byte, MaxUDPPayload)
	data := make([]byte, 1200)
	hdr := make([]byte, 16)
	BuildStreamHead(hdr, 1, 0)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		fb := NewFrameBuilder(buf[:0])
		bodyLen := 16 + len(data)
		fb.Add(FrameStream, 0, bodyLen, func(d []byte) {
			copy(d[:16], hdr)
			copy(d[16:], data)
		})
	}
}

func BenchmarkFrameBuilderAck(b *testing.B) {
	buf := make([]byte, MaxUDPPayload)
	var a AckFrame
	a.AckDelayUS = 1000
	a.Count = 2
	a.Ranges[0] = [2]uint64{50, 100}
	a.Ranges[1] = [2]uint64{0, 20}
	bodyLen := AckBodyLen(a.Count)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		fb := NewFrameBuilder(buf[:0])
		body := make([]byte, bodyLen)
		BuildAckBody(body, &a)
		fb.Add(FrameAck, 0, len(body), func(d []byte) {
			copy(d, body)
		})
	}
}

func BenchmarkFrameBuilderStreamReserve(b *testing.B) {
	buf := make([]byte, MaxUDPPayload)
	data := make([]byte, 1200)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		fb := NewFrameBuilder(buf[:0])
		bodyLen := 16 + len(data)
		dst, ok := fb.Reserve(FrameStream, 0, bodyLen)
		if !ok {
			b.Fatal("reserve failed")
		}
		BuildStreamHead(dst[:16], 1, 0)
		copy(dst[16:], data)
	}
}

func BenchmarkFrameBuilderAckReserve(b *testing.B) {
	buf := make([]byte, MaxUDPPayload)
	var a AckFrame
	a.AckDelayUS = 1000
	a.Count = 2
	a.Ranges[0] = [2]uint64{50, 100}
	a.Ranges[1] = [2]uint64{0, 20}
	bodyLen := AckBodyLen(a.Count)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		fb := NewFrameBuilder(buf[:0])
		dst, ok := fb.Reserve(FrameAck, 0, bodyLen)
		if !ok {
			b.Fatal("reserve failed")
		}
		BuildAckBody(dst, &a)
	}
}
