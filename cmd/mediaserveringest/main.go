package main

import (
	"context"
	"flag"
	"github.com/je4/indexer/v2/pkg/indexer"
	mediaserverdbClient "github.com/je4/mediaserverdb/v2/pkg/client"
	"github.com/je4/mediaserverdb/v2/pkg/mediaserverdbproto"
	"github.com/je4/mediaserveringest/v2/config"
	"github.com/je4/mediaserveringest/v2/internal"
	"github.com/je4/trustutil/v2/pkg/loader"
	"github.com/je4/utils/v2/pkg/zLogger"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/emptypb"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
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

	clientCert, clientLoader, err := loader.CreateClientLoader(conf.Client, logger)
	if err != nil {
		logger.Panic().Msgf("cannot create client loader: %v", err)
	}
	defer clientLoader.Close()

	if _, ok := conf.GRPCClient["mediaserverdb"]; !ok {
		logger.Fatal().Msg("no mediaserverdb grpc client defined")
	}
	dbClient, dbClientConn, err := mediaserverdbClient.CreateClient(conf.GRPCClient["mediaserverdb"], clientCert)
	if err != nil {
		logger.Panic().Msgf("cannot create mediaserverdb grpc client: %v", err)
	}
	defer dbClientConn.Close()
	if resp, err := dbClient.Ping(context.Background(), &emptypb.Empty{}); err != nil {
		logger.Error().Msgf("cannot ping mediaserverdb: %v", err)
	} else {
		if resp.GetStatus() != mediaserverdbproto.ResultStatus_OK {
			logger.Error().Msgf("cannot ping mediaserverdb: %v", resp.GetStatus())
		} else {
			logger.Info().Msgf("mediaserverdb ping response: %s", resp.GetMessage())
		}
	}

	var fss = map[string]fs.FS{"internal": internal.InternalFS}

	indexerActions, err := indexer.InitActionDispatcher(fss, *conf.Indexer, zLogger.NewZWrapper(logger))
	if err != nil {
		logger.Panic().Err(err).Msg("cannot init indexer")
	}

	_ = indexerActions
}
