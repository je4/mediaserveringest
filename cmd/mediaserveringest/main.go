package main

import (
	"flag"
	"fmt"
	"github.com/je4/filesystem/v2/pkg/vfsrw"
	"github.com/je4/indexer/v2/pkg/indexer"
	"github.com/je4/mediaserveringest/v2/config"
	"github.com/je4/mediaserveringest/v2/internal"
	"github.com/je4/mediaserveringest/v2/pkg/ingest"
	mediaserverproto "github.com/je4/mediaserverproto/v2/pkg/mediaserver/proto"
	"github.com/je4/miniresolver/v2/pkg/resolver"
	"github.com/je4/trustutil/v2/pkg/loader"
	"github.com/je4/utils/v2/pkg/zLogger"
	"github.com/rs/zerolog"
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

	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("cannot get hostname: %v", err)
	}

	output := zerolog.ConsoleWriter{Out: out, TimeFormat: time.RFC3339}
	_logger := zerolog.New(output).With().Timestamp().Str("service", "mediaserveringest"). /*.Array("addrs", zLogger.StringArray(addrStr))*/ Str("host", hostname).Str("addr", conf.LocalAddr).Logger()
	_logger.Level(zLogger.LogLevel(conf.LogLevel))
	var logger zLogger.ZLogger = &_logger

	clientTLSConfig, clientLoader, err := loader.CreateClientLoader(conf.ClientTLS, logger)
	if err != nil {
		logger.Panic().Msgf("cannot create client loader: %v", err)
	}
	defer clientLoader.Close()

	logger.Info().Msgf("resolver address is %s", conf.ResolverAddr)
	miniResolverClient, err := resolver.NewMiniresolverClient(conf.ResolverAddr, conf.GRPCClient, clientTLSConfig, nil, time.Duration(conf.ResolverTimeout), time.Duration(conf.ResolverNotFoundTimeout), logger)
	if err != nil {
		logger.Fatal().Msgf("cannot create resolver client: %v", err)
	}
	defer miniResolverClient.Close()

	dbClient, err := resolver.NewClient[mediaserverproto.DatabaseClient](miniResolverClient, mediaserverproto.NewDatabaseClient, mediaserverproto.Database_ServiceDesc.ServiceName)
	if err != nil {
		logger.Panic().Msgf("cannot create mediaserverdb grpc client: %v", err)
	}
	resolver.DoPing(dbClient, logger)

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
