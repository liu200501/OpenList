package stream_test

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

// TestReadAtSeekerRangeIsBounded guards against the old behavior where
// getReaderAtOffset used Range{Start: off, Length: -1}, i.e. "read everything
// to EOF" on every fresh range request. On archive-in-crypt previews that
// repeatedly pulled the whole archive from the cloud. Here every requested
// range must be finite and clamped to conf.MaxBlockLimit / remaining bytes.
func TestReadAtSeekerRangeIsBounded(t *testing.T) {
	data := randomData(8 * 1024 * 1024)
	const limit = 64 * 1024

	prev := conf.MaxBlockLimit
	t.Cleanup(func() { conf.MaxBlockLimit = prev })
	conf.MaxBlockLimit = limit

	var (
		gets     atomic.Int64
		maxLen   atomic.Int64
		badRange atomic.Int64
	)
	rr := stream.RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
		gets.Add(1)
		if r.Length < 0 {
			badRange.Add(1) // negative length == read to EOF
			r.Length = int64(len(data)) - r.Start
		}
		// Length may exceed the cap only when a single ReadAt(p) is larger;
		// it must still be finite and within the file.
		if r.Start < 0 || r.Start > int64(len(data)) || r.Start+r.Length > int64(len(data)) {
			badRange.Add(1)
			r.Length = int64(len(data)) - r.Start
		}
		if l := maxLen.Load(); r.Length > l {
			maxLen.Store(r.Length)
		}
		return io.NopCloser(bytes.NewReader(data[r.Start : r.Start+r.Length])), nil
	})
	ss, err := stream.NewSeekableStream(&stream.FileStream{
		Obj: &model.Object{Size: int64(len(data))},
		Ctx: context.Background(),
	}, &model.Link{RangeReader: rr, ContentLength: int64(len(data))})
	if err != nil {
		t.Fatalf("NewSeekableStream() error = %v", err)
	}
	ra, err := stream.NewReadAtSeeker(ss, 0, true)
	if err != nil {
		t.Fatalf("NewReadAtSeeker() error = %v", err)
	}

	// A single ReadAt larger than the cap must still be satisfied in one call,
	// otherwise io.ReadFull turns a short range into a spurious EOF. Do it first
	// so it cannot hit a parked continuation reader with too few bytes left.
	bigBuf := make([]byte, limit+8192)
	readAtFull(t, ra, data, 1024*1024, bigBuf)

	// Far-apart reads force several fresh range requests (outside reuse window).
	buf := make([]byte, 4096)
	for _, off := range []int64{0, 5 * 1024 * 1024, 2 * 1024 * 1024, 7 * 1024 * 1024} {
		readAtFull(t, ra, data, off, buf)
	}
	if gets.Load() == 0 {
		t.Fatal("no range requests were issued")
	}
	if bad := badRange.Load(); bad != 0 {
		t.Fatalf("%d range requests were unbounded or out of file bounds", bad)
	}
	if maxLen.Load() <= int64(limit) {
		t.Fatalf("expected at least one range request larger than the cap (the big ReadAt), max = %d", maxLen.Load())
	}
}
