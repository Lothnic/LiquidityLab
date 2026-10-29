package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/replayjobs"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

// Replay gets a separate two-connection pool and one calculation slot. Public
// archive bytes are selected by the operator; requests can select only content
// IDs. These bounds do not establish shared-account or deployment capacity.
func openPaperReplayJobs(ctx context.Context, opt paperServerOptions, dsn string) (*replayjobs.Service, *pg.Store, error) {
	if opt.replayArchive == "" {
		return nil, nil, nil
	}
	info, err := os.Lstat(opt.replayArchive)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("replay archive must be an existing ordinary directory")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, errors.New("cannot create replay worker identity")
	}
	op, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	store, err := pg.OpenWithOptions(op, dsn, opt.schema, pg.Options{MaxConnections: 2, ApplicationName: "liquiditylab-replay-jobs"})
	if err != nil {
		return nil, nil, err
	}
	service, err := replayjobs.New(replayjobs.Config{Queue: store, ArchiveRoot: opt.replayArchive, WorkerID: "replay-" + hex.EncodeToString(nonce[:])})
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return service, store, nil
}

func (p *paperDaemon) closeReplayJobs() error {
	if p.replayStore == nil {
		return nil
	}
	p.stopReplay()
	select {
	case <-p.replayDone:
		p.replayStore.Close()
		return nil
	case <-time.After(5 * time.Second):
		// Do not block financial shutdown indefinitely. Keep ownership of the
		// pool until its calculation exits, then reap it. The unfinished job
		// can be reclaimed only after its database lease expires.
		go func() { <-p.replayDone; p.replayStore.Close() }()
		return errors.New("replay worker shutdown exceeded deadline; unfinished analytical jobs retained")
	}
}
