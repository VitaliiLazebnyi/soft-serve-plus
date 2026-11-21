package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/matryer/is"
)

// batchRecorder is an LFSClient that records each batch it is asked for and
// reports every object as unavailable, so nothing is stored.
type batchRecorder struct {
	batches [][]string
}

func (c *batchRecorder) Download(_ context.Context, pointers []lfs.Pointer, cb lfs.DownloadCallback) error {
	var oids []string
	for _, p := range pointers {
		oids = append(oids, fmt.Sprintf("%s/%d", p.Oid[:2], p.Size))
		if err := cb(p, nil, errors.New("unavailable")); err != nil {
			return err
		}
	}
	c.batches = append(c.batches, oids)
	return nil
}

// Objects are asked for lfsBatchSize at a time, each oid once per batch: a
// second pointer with an oid already in the batch waits for a later one.
func TestLFSSyncBatches(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	client := &batchRecorder{}
	s := &lfsSync{
		ctx:      context.Background(),
		repo:     &repo{name: "r"},
		client:   client,
		logger:   log.New(io.Discard),
		batched:  make(map[string]bool),
		verified: make(map[string]bool),
	}
	oid := func(i int) string { return fmt.Sprintf("%02x%062d", i, 0) }

	for i := range lfsBatchSize + 1 {
		is.NoErr(s.queue(lfs.Pointer{Oid: oid(i), Size: 1}))
		if i == 0 {
			// The same oid with another size, as a malformed pointer has.
			is.NoErr(s.queue(lfs.Pointer{Oid: oid(0), Size: 2}))
		}
	}
	is.Equal(len(client.batches), 1) // the first batch went when full
	is.Equal(len(client.batches[0]), lfsBatchSize)
	is.Equal(len(s.deferred), 1)

	is.NoErr(s.flush())
	is.Equal(len(client.batches), 2)
	is.Equal(client.batches[1], []string{"14/1"})

	is.NoErr(s.queue(s.deferred[0]))
	is.NoErr(s.flush())
	is.Equal(client.batches[2], []string{"00/2"})
	is.Equal(s.unavailable, lfsBatchSize+2)
	is.NoErr(s.flush()) // an empty batch asks for nothing
	is.Equal(len(client.batches), 3)
}
