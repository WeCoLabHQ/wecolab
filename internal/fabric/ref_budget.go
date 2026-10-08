package fabric

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Leave room for controller state, parser copies and outgoing pack encoding in
// Warden's 256 MiB container. Exceeding this budget retains the source, never
// truncates history. Both compressed bytes and aggregate expanded data are capped.
const refHistoryBytes = 8 << 20
const refHistoryObjects = 16384

var errRefHistoryBudget = errors.New("Git history exceeds the in-memory budget; source retained")

type boundedRefResponse struct {
	io.ReadCloser
	remaining int
}

func (r *boundedRefResponse) Read(b []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errRefHistoryBudget
	}
	n, err := r.ReadCloser.Read(b[:min(len(b), r.remaining)])
	r.remaining -= n
	return n, err
}

// Checking SetEncodedObject is too late: go-git allocates inflated objects and
// delta buffers first. PackfileWriter intercepts the pack before that parser.
type boundedRefStorage struct {
	*memory.Storage
	ctx context.Context
}

func (s *boundedRefStorage) PackfileWriter() (io.WriteCloser, error) {
	return &boundedRefPack{store: s}, nil
}

type boundedRefPack struct {
	store *boundedRefStorage
	data  bytes.Buffer
	err   error
}

func (p *boundedRefPack) Write(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	if p.err = p.store.ctx.Err(); p.err != nil {
		return 0, p.err
	}
	if len(b) > refHistoryBytes-p.data.Len() {
		p.err = errRefHistoryBudget
		return 0, p.err
	}
	return p.data.Write(b)
}

func (p *boundedRefPack) Close() error {
	if p.err != nil {
		return p.err
	}
	if err := preflightRefPack(p.store.ctx, p.data.Bytes()); err != nil {
		return err
	}
	// A seekable, preflighted pack avoids go-git's non-seekable delta buffer map.
	// Pass the inner store, not this adapter, to enter the normal parser once.
	return packfile.UpdateObjectStorage(p.store.Storage, bytes.NewReader(p.data.Bytes()))
}

// deltaPrefix retains only the two unsigned LEB128 lengths, not the delta body.
// The pinned scanner rejects inflation beyond the pack's declared object length.
type deltaPrefix struct {
	ctx   context.Context
	bytes [20]byte
	n     int
}

func (p *deltaPrefix) Write(b []byte) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	p.n += copy(p.bytes[p.n:], b)
	return len(b), nil
}

func preflightRefPack(ctx context.Context, data []byte) error {
	if len(data) > refHistoryBytes {
		return errRefHistoryBudget
	}
	scanner := packfile.NewScanner(bytes.NewReader(data))
	_, count, err := scanner.Header()
	if err != nil {
		return err
	}
	if count > refHistoryObjects {
		return errRefHistoryBudget
	}
	remaining := int64(refHistoryBytes)
	for range count {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := scanner.NextObjectHeader()
		if err != nil {
			return err
		}
		if header.Length < 0 || header.Length > remaining {
			return errRefHistoryBudget
		}
		remaining -= header.Length
		prefix := deltaPrefix{ctx: ctx}
		written, _, err := scanner.NextObject(&prefix)
		if err != nil {
			return err
		}
		if written != header.Length {
			return errors.New("invalid Git object length")
		}
		if header.Type == plumbing.OFSDeltaObject || header.Type == plumbing.REFDeltaObject {
			base, n := binary.Uvarint(prefix.bytes[:prefix.n])
			if n <= 0 {
				return errors.New("invalid Git delta base length")
			}
			target, n := binary.Uvarint(prefix.bytes[n:prefix.n])
			if n <= 0 {
				return errors.New("invalid Git delta target length")
			}
			if base > refHistoryBytes || target > uint64(remaining) {
				return errRefHistoryBudget
			}
			remaining -= int64(target)
		}
	}
	return nil
}
