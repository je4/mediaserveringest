package main

import (
	"emperror.dev/errors"
	"github.com/BurntSushi/toml"
	"github.com/blend/go-sdk/configutil"
	"github.com/je4/indexer/v2/pkg/indexer"
	"github.com/je4/trustutil/v2/pkg/loader"
	"io/fs"
	"os"
)

type MediaserverIngestConfig struct {
	LocalAddr string `toml:"localaddr"`
	LogFile   string `toml:"logfile"`
	LogLevel  string `toml:"loglevel"`

	IngestTimeout   configutil.Duration `toml:"ingesttimeout"`
	ConcurrentTasks int                 `toml:"concurrenttasks"`
	GRPCClient      map[string]string   `toml:"grpcclient"`
	Client          *loader.TLSConfig   `toml:"client"`

	Indexer *indexer.IndexerConfig
}

func LoadMediaserverIngestConfig(fSys fs.FS, fp string, conf *MediaserverIngestConfig) error {
	if _, err := fs.Stat(fSys, fp); err != nil {
		path, err := os.Getwd()
		if err != nil {
			return errors.Wrap(err, "cannot get current working directory")
		}
		fSys = os.DirFS(path)
		fp = "mediaserveringest.toml"
	}
	data, err := fs.ReadFile(fSys, fp)
	if err != nil {
		return errors.Wrapf(err, "cannot read file [%v] %s", fSys, fp)
	}
	_, err = toml.Decode(string(data), conf)
	if err != nil {
		return errors.Wrapf(err, "error loading config file %v", fp)
	}
	return nil

}
