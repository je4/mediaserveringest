package main

import (
	"emperror.dev/errors"
	"github.com/BurntSushi/toml"
	loaderConfig "github.com/je4/certloader/v2/pkg/loader"
	"github.com/je4/filesystem/v3/pkg/vfsrw"
	"github.com/je4/indexer/v3/pkg/indexer"
	"github.com/je4/utils/v2/pkg/config"
	"github.com/je4/utils/v2/pkg/stashconfig"
	"io/fs"
	"os"
)

type MediaserverIngestConfig struct {
	LocalAddr               string          `toml:"localaddr"`
	Domains                 []string        `toml:"domains"`
	ResolverAddr            string          `toml:"resolveraddr"`
	ResolverTimeout         config.Duration `toml:"resolvertimeout"`
	ResolverNotFoundTimeout config.Duration `toml:"resolvernotfoundtimeout"`

	IngestTimeout   config.Duration      `toml:"ingesttimeout"`
	IngestWait      config.Duration      `toml:"ingestwait"`
	ConcurrentTasks int                  `toml:"concurrenttasks"`
	GRPCClient      map[string]string    `toml:"grpcclient"`
	ClientTLS       *loaderConfig.Config `toml:"client"`

	Indexer *indexer.IndexerConfig

	VFS map[string]*vfsrw.VFS `toml:"vfs"`
	Log stashconfig.Config    `toml:"log"`
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
