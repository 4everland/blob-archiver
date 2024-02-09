package main

import (
	"context"
	"fmt"
	"os"

	"github.com/base-org/blob-archiver/archiver/flags"
	"github.com/base-org/blob-archiver/archiver/metrics"
	"github.com/base-org/blob-archiver/archiver/service"
	"github.com/base-org/blob-archiver/common/beacon"
	"github.com/base-org/blob-archiver/common/storage"
	opservice "github.com/ethereum-optimism/optimism/op-service"
	"github.com/ethereum-optimism/optimism/op-service/cliapp"
	oplog "github.com/ethereum-optimism/optimism/op-service/log"
	opmetrics "github.com/ethereum-optimism/optimism/op-service/metrics"
	"github.com/ethereum/go-ethereum/log"
	"github.com/urfave/cli/v2"
)

var (
	Version   = "v0.0.1"
	GitCommit = ""
	GitDate   = ""
)

func main() {
	oplog.SetupDefaults()

	app := cli.NewApp()
	app.Flags = cliapp.ProtectFlags(flags.Flags)
	app.Version = opservice.FormatVersion(Version, GitCommit, GitDate, "")
	app.Name = "blob-archiver"
	app.Usage = "Archiver service for Ethereum blobs"
	app.Description = "Service for fetching blobs and archiving them to a datastore"
	app.Action = cliapp.LifecycleCmd(Main())

	err := app.Run(os.Args)
	if err != nil {
		log.Crit("Application failed", "message", err)
	}
}

// Main is the entrypoint into the Archiver.
// This method returns a cliapp.LifecycleAction, to create an op-service CLI-lifecycle-managed archiver.
func Main() cliapp.LifecycleAction {
	return func(cliCtx *cli.Context, closeApp context.CancelCauseFunc) (cliapp.Lifecycle, error) {
		cfg := flags.ReadConfig(cliCtx)

		if err := cfg.Check(); err != nil {
			return nil, fmt.Errorf("invalid CLI flags: %w", err)
		}

		l := oplog.NewLogger(oplog.AppOut(cliCtx), cfg.LogConfig)
		oplog.SetGlobalLogHandler(l.GetHandler())
		opservice.ValidateEnvVars(flags.EnvVarPrefix, flags.Flags, l)

		registry := opmetrics.NewRegistry()
		m := metrics.NewMetrics(registry)

		beaconClient, err := beacon.NewBeaconClient(context.Background(), cfg.BeaconConfig)
		if err != nil {
			return nil, err
		}

		storageClient, err := storage.NewStorage(cfg.StorageConfig, l)
		if err != nil {
			return nil, err
		}

		l.Info("Initializing Archiver Service")
		return service.NewArchiverService(l, cfg, storageClient, beaconClient, m, registry)
	}
}
