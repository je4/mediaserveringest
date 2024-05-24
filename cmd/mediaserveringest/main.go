package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/je4/filesystem/v2/pkg/vfsrw"
	genericproto "github.com/je4/genericproto/v2/pkg/generic/proto"
	"github.com/je4/indexer/v2/pkg/indexer"
	"github.com/je4/mediaserveringest/v2/config"
	"github.com/je4/mediaserveringest/v2/internal"
	"github.com/je4/mediaserveringest/v2/pkg/ingest"
	mediaserverdbClient "github.com/je4/mediaserverproto/v2/pkg/mediaserverdb/client"
	mediaserverdbproto "github.com/je4/mediaserverproto/v2/pkg/mediaserverdb/proto"
	miniresolverClient "github.com/je4/miniresolver/v2/pkg/client"
	resolverhelper "github.com/je4/miniresolver/v2/pkg/grpchelper"
	"github.com/je4/trustutil/v2/pkg/loader"
	"github.com/je4/utils/v2/pkg/zLogger"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/emptypb"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var cfg = flag.String("config", "", "location of toml configuration file")

func main() {
	flag.Parse()
	var cfgFS fs.FS
	var cfgFile string
	if *cfg != "" {
		cfgFS = os.DirFS(filepath.Dir(*cfg))
		cfgFile = filepath.Base(*cfg)
	} else {
		cfgFS = config.ConfigFS
		cfgFile = "mediaserveringest.toml"
	}
	conf := &MediaserverIngestConfig{
		LocalAddr: "localhost:8442",
		LogLevel:  "DEBUG",
	}
	if err := LoadMediaserverIngestConfig(cfgFS, cfgFile, conf); err != nil {
		log.Fatalf("cannot load toml from [%v] %s: %v", cfgFS, cfgFile, err)
	}
	// create logger instance
	var out io.Writer = os.Stdout
	if conf.LogFile != "" {
		fp, err := os.OpenFile(conf.LogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("cannot open logfile %s: %v", conf.LogFile, err)
		}
		defer fp.Close()
		out = fp
	}

	output := zerolog.ConsoleWriter{Out: out, TimeFormat: time.RFC3339}
	_logger := zerolog.New(output).With().Timestamp().Logger()
	_logger.Level(zLogger.LogLevel(conf.LogLevel))
	var logger zLogger.ZLogger = &_logger

	clientTLSConfig, clientLoader, err := loader.CreateClientLoader(conf.ClientTLS, logger)
	if err != nil {
		logger.Panic().Msgf("cannot create client loader: %v", err)
	}
	defer clientLoader.Close()

	var dbClientAddr string
	if conf.ResolverAddr != "" {
		dbClientAddr = resolverhelper.GetAddress(mediaserverdbproto.DBController_Ping_FullMethodName)
		logger.Info().Msgf("resolver address is %s", conf.ResolverAddr)
		miniResolverClient, miniResolverCloser, err := miniresolverClient.CreateClient(conf.ResolverAddr, clientTLSConfig)
		if err != nil {
			logger.Fatal().Msgf("cannot create resolver client: %v", err)
		}
		defer miniResolverCloser.Close()
		resolverhelper.RegisterResolver(miniResolverClient, time.Duration(conf.ResolverTimeout), time.Duration(conf.ResolverNotFoundTimeout), logger)
	} else {
		if _, ok := conf.GRPCClient["mediaserverdb"]; !ok {
			logger.Fatal().Msg("no mediaserverdb grpc client defined")
		}
		dbClientAddr = conf.GRPCClient["mediaserverdb"]
	}

	dbClient, dbClientConn, err := mediaserverdbClient.CreateClient(dbClientAddr, clientTLSConfig)
	if err != nil {
		logger.Panic().Msgf("cannot create mediaserverdb grpc client: %v", err)
	}
	defer dbClientConn.Close()
	if resp, err := dbClient.Ping(context.Background(), &emptypb.Empty{}); err != nil {
		logger.Error().Msgf("cannot ping mediaserverdb: %v", err)
	} else {
		if resp.GetStatus() != genericproto.ResultStatus_OK {
			logger.Error().Msgf("cannot ping mediaserverdb: %v", resp.GetStatus())
		} else {
			logger.Info().Msgf("mediaserverdb ping response: %s", resp.GetMessage())
		}
	}

	vfs, err := vfsrw.NewFS(conf.VFS, zLogger.NewZWrapper(logger))
	if err != nil {
		logger.Panic().Err(err).Msg("cannot create vfs")
	}
	defer func() {
		if err := vfs.Close(); err != nil {
			logger.Error().Err(err).Msg("cannot close vfs")
		}
	}()

	var fss = map[string]fs.FS{"internal": internal.InternalFS}

	indexerActions, err := indexer.InitActionDispatcher(fss, *conf.Indexer, zLogger.NewZWrapper(logger))
	if err != nil {
		logger.Panic().Err(err).Msg("cannot init indexer")
	}

	ingester, err := ingest.NewIngester(indexerActions, dbClient, vfs, conf.ConcurrentTasks, time.Duration(conf.IngestTimeout), time.Duration(conf.IngestWait), logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("cannot create ingester")
	}
	if err := ingester.Start(); err != nil {
		logger.Fatal().Err(err).Msg("cannot start ingester")
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	fmt.Println("press ctrl+c to stop server")
	s := <-done
	fmt.Println("got signal:", s)

	defer ingester.Close()

}
