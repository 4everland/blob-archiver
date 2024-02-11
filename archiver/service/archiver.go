package service

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	client "github.com/attestantio/go-eth2-client"
	"github.com/attestantio/go-eth2-client/api"
	v1 "github.com/attestantio/go-eth2-client/api/v1"
	"github.com/base-org/blob-archiver/archiver/flags"
	"github.com/base-org/blob-archiver/archiver/metrics"
	"github.com/base-org/blob-archiver/common/storage"
	"github.com/ethereum-optimism/optimism/op-service/httputil"
	opmetrics "github.com/ethereum-optimism/optimism/op-service/metrics"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

const backfillErrorRetryInterval = 5 * time.Second

var ErrAlreadyStopped = errors.New("already stopped")

type BeaconClient interface {
	client.BlobSidecarsProvider
	client.BeaconBlockHeadersProvider
}

func NewService(l log.Logger, cfg flags.ArchiverConfig, dataStoreClient storage.DataStore, client BeaconClient, m metrics.Metricer) (*ArchiverService, error) {
	return &ArchiverService{
		log:             l,
		cfg:             cfg,
		dataStoreClient: dataStoreClient,
		metrics:         m,
		stopCh:          make(chan struct{}),
		beaconClient:    client,
	}, nil
}

type ArchiverService struct {
	stopped         atomic.Bool
	stopCh          chan struct{}
	log             log.Logger
	dataStoreClient storage.DataStore
	beaconClient    BeaconClient
	metricsServer   *httputil.HTTPServer
	cfg             flags.ArchiverConfig
	metrics         metrics.Metricer
}

func (a *ArchiverService) Start(ctx context.Context) error {
	if a.cfg.MetricsConfig.Enabled {
		a.log.Info("starting metrics server", "addr", a.cfg.MetricsConfig.ListenAddr, "port", a.cfg.MetricsConfig.ListenPort)
		srv, err := opmetrics.StartServer(a.metrics.Registry(), a.cfg.MetricsConfig.ListenAddr, a.cfg.MetricsConfig.ListenPort)
		if err != nil {
			return err
		}

		a.log.Info("started metrics server", "addr", srv.Addr())
		a.metricsServer = srv
	}

	currentBlob, _, err := a.persistBlobsForBlockToS3(ctx, "head")
	if err != nil {
		a.log.Error("failed to seed archiver with initial block", "err", err)
		return err
	}

	go a.backfillBlobs(ctx, currentBlob)

	return a.trackLatestBlocks(ctx)
}

func (a *ArchiverService) persistBlobsForBlockToS3(ctx context.Context, blockIdentifier string) (*v1.BeaconBlockHeader, bool, error) {
	currentHeader, err := a.beaconClient.BeaconBlockHeader(ctx, &api.BeaconBlockHeaderOpts{
		Block: blockIdentifier,
	})

	if err != nil {
		a.log.Error("failed to fetch latest beacon block header", "err", err)
		return nil, false, err
	}

	exists, err := a.dataStoreClient.Exists(ctx, common.Hash(currentHeader.Data.Root))
	if err != nil {
		a.log.Error("failed to check if blob exists", "err", err)
		return nil, false, err
	}

	if exists {
		a.log.Debug("blob already exists", "hash", currentHeader.Data.Root)
		return currentHeader.Data, true, nil
	}

	blobSidecars, err := a.beaconClient.BlobSidecars(ctx, &api.BlobSidecarsOpts{
		Block: currentHeader.Data.Root.String(),
	})

	if err != nil {
		a.log.Error("failed to fetch blob sidecars", "err", err)
		return nil, false, err
	}

	a.log.Debug("fetched blob sidecars", "count", len(blobSidecars.Data))

	blobData := storage.BlobData{
		Header: storage.Header{
			BeaconBlockHash: common.Hash(currentHeader.Data.Root),
		},
		BlobSidecars: storage.BlobSidecars{Data: blobSidecars.Data},
	}

	err = a.dataStoreClient.Write(ctx, blobData)

	if err != nil {
		a.log.Error("failed to write blob", "err", err)
		return nil, false, err
	}

	a.metrics.RecordStoredBlobs(len(blobSidecars.Data))

	return currentHeader.Data, false, nil
}

func (a *ArchiverService) Stop(ctx context.Context) error {
	if a.stopped.Load() {
		return ErrAlreadyStopped
	}
	a.log.Info("Stopping Archiver")
	a.stopped.Store(true)

	close(a.stopCh)

	if a.metricsServer != nil {
		if err := a.metricsServer.Stop(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (a *ArchiverService) Stopped() bool {
	return a.stopped.Load()
}

func (a *ArchiverService) backfillBlobs(ctx context.Context, latest *v1.BeaconBlockHeader) {
	current, alreadyExists, err := latest, false, error(nil)

	for !alreadyExists {
		if common.Hash(current.Root) == a.cfg.OriginBlock {
			a.log.Info("reached origin block", "hash", current.Root.String())
			return
		}

		previous := current
		current, alreadyExists, err = a.persistBlobsForBlockToS3(ctx, previous.Header.Message.ParentRoot.String())
		if err != nil {
			a.log.Error("failed to persist blobs for block, will retry", "err", err, "hash", previous.Header.Message.ParentRoot.String())
			// Revert back to block we failed to fetch
			current = previous
			time.Sleep(backfillErrorRetryInterval)
			continue
		}

		if !alreadyExists {
			a.metrics.RecordProcessedBlock(metrics.BlockSourceBackfill)
		}
	}

	a.log.Info("backfill complete", "endHash", current.Root.String(), "startHash", latest.Root.String())
}

func (a *ArchiverService) trackLatestBlocks(ctx context.Context) error {
	t := time.NewTicker(a.cfg.PollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-a.stopCh:
			return nil
		case <-t.C:
			a.processBlocksUntilKnownBlock(ctx)
		}
	}
}

func (a *ArchiverService) processBlocksUntilKnownBlock(ctx context.Context) {
	a.log.Debug("refreshing live data")

	var start *v1.BeaconBlockHeader
	currentBlockId := "head"

	for {
		current, alreadyExisted, err := a.persistBlobsForBlockToS3(ctx, currentBlockId)

		if err != nil {
			a.log.Error("failed to update live blobs for block", "err", err, "blockId", currentBlockId)
			return
		}

		if start == nil {
			start = current
		}

		if !alreadyExisted {
			a.metrics.RecordProcessedBlock(metrics.BlockSourceLive)
		} else {
			a.log.Debug("blob already exists", "hash", current.Root.String())
			break
		}

		currentBlockId = current.Header.Message.ParentRoot.String()
	}

	a.log.Info("live data refreshed", "startHash", start.Root.String(), "endHash", currentBlockId)
}
