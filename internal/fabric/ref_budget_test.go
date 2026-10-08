package fabric

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestRefFetchRejectsOversizedInflatedHistory(t *testing.T) {
	r := newRefRepo(t)
	tip := r.commit(t, "", strings.Repeat("compressible-history", (8<<20)/20+1))
	r.set(t, "refs/heads/main", tip)
	_, err := r.git.FetchRefs(context.Background(), map[string]string{"refs/heads/main": tip})
	if err == nil || !strings.Contains(err.Error(), "memory budget") {
		t.Fatalf("oversized Git history must fail closed before decoding into Warden memory: %v", err)
	}
	if r.ref(t, "refs/heads/main") != tip {
		t.Fatal("rejected transfer modified source history")
	}
}

func TestRefPreflightBoundsDeltaExpansionBeforeParsing(t *testing.T) {
	var data bytes.Buffer
	data.WriteString("PACK")
	_ = binary.Write(&data, binary.BigEndian, uint32(2))
	_ = binary.Write(&data, binary.BigEndian, uint32(1))
	var delta bytes.Buffer
	var n [10]byte
	delta.Write(n[:binary.PutUvarint(n[:], 1)])
	delta.Write(n[:binary.PutUvarint(n[:], refHistoryBytes+1)])
	data.WriteByte(7<<4 | byte(delta.Len())) // REF_DELTA with a tiny program
	data.Write(make([]byte, 20))
	compressed := zlib.NewWriter(&data)
	_, _ = compressed.Write(delta.Bytes())
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := preflightRefPack(context.Background(), data.Bytes()); !errors.Is(err, errRefHistoryBudget) {
		t.Fatalf("tiny delta declaring oversized output reached the allocator: %v", err)
	}

	header := append([]byte(nil), data.Bytes()[:12]...)
	binary.BigEndian.PutUint32(header[8:], refHistoryObjects+1)
	if err := preflightRefPack(context.Background(), header); !errors.Is(err, errRefHistoryBudget) {
		t.Fatalf("oversized object table reached the allocator: %v", err)
	}
}
