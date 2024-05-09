package ingest

import (
	"context"
	"emperror.dev/errors"
	"github.com/je4/filesystem/v2/pkg/writefs"
	"github.com/je4/indexer/v2/pkg/indexer"
	"github.com/je4/mediaserverdb/v2/pkg/mediaserverdbproto"
	"github.com/je4/utils/v2/pkg/zLogger"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

func NewIngester(indexer *indexer.ActionDispatcher, dbClient mediaserverdbproto.DBControllerClient, vfs fs.FS, concurrentWorkers int, ingestTimeout time.Duration, ingestWait time.Duration, logger zLogger.ZLogger) (*Ingester, error) {
	if concurrentWorkers < 1 {
		return nil, errors.New("concurrentWorkers must be at least 1")
	}
	if ingestTimeout < 1 {
		return nil, errors.New("ingestTimeout must not be 0")
	}
	i := &Ingester{
		indexer:       indexer,
		dbClient:      dbClient,
		end:           make(chan bool),
		jobChan:       make(chan *JobStruct),
		ingestTimeout: ingestTimeout,
		ingestWait:    ingestWait,
		logger:        logger,
		vfs:           vfs,
	}
	i.jobChan, i.worker = NewWorkerPool(concurrentWorkers, ingestTimeout, i.doIngest, logger)

	return i, nil
}

type Ingester struct {
	indexer       *indexer.ActionDispatcher
	dbClient      mediaserverdbproto.DBControllerClient
	end           chan bool
	worker        io.Closer
	jobChan       chan *JobStruct
	ingestTimeout time.Duration
	ingestWait    time.Duration
	logger        zLogger.ZLogger
	vfs           fs.FS
}
type WriterNopcloser struct {
	io.Writer
}

func (WriterNopcloser) Close() error { return nil }

func (i *Ingester) doIngest(job *JobStruct) error {
	i.logger.Debug().Msgf("ingest %s/%s", job.collection.Name, job.signature)

	var targetWriter io.WriteCloser
	var err error
	var cachePath string
	switch job.ingestType {
	case IngestType_KEEP:
		cachePath = job.urn
		targetWriter = WriterNopcloser{io.Discard}
		i.logger.Debug().Msgf("keep %s/%s", job.collection.Name, job.signature)
	case IngestType_COPY:
		cacheName := createCacheName(job.collection.Name, job.signature, job.urn)
		cachePath = strings.Join([]string{job.collection.Storage.Datadir, cacheName}, "/")
		fullpath := strings.Join([]string{job.collection.Storage.Filebase, cachePath}, "/")
		targetWriter, err = writefs.Create(i.vfs, fullpath)
		i.logger.Debug().Msgf("copy %s/%s -> %s", job.collection.Name, job.signature, fullpath)
	case IngestType_MOVE:
		cacheName := createCacheName(job.collection.Name, job.signature, job.urn)
		cachePath = strings.Join([]string{job.collection.Storage.Datadir, cacheName}, "/")
		fullpath := strings.Join([]string{job.collection.Storage.Filebase, cachePath}, "/")
		targetWriter, err = writefs.Create(i.vfs, fullpath)
		i.logger.Debug().Msgf("move %s/%s -> %s", job.collection.Name, job.signature, fullpath)
	default:
		return errors.Errorf("unknown ingest type %d", job.ingestType)
	}
	if err != nil {
		return errors.Wrapf(err, "cannot create %s", job.urn)
	}

	sourceReader, err := i.vfs.Open(job.urn)
	if err != nil {
		targetWriter.Close()
		return errors.Wrapf(err, "cannot open %s", job.urn)
	}
	defer sourceReader.Close()

	name := filepath.Base(job.urn)
	pr, pw := io.Pipe()
	var done = make(chan error)
	go func() {
		var resultErr error
		i.logger.Debug().Msgf("start copying %s", job.urn)
		if _, err := io.Copy(io.MultiWriter(targetWriter, pw), sourceReader); err != nil {
			resultErr = errors.Wrapf(err, "cannot copy %s", job.urn)
		}
		pw.Close()
		targetWriter.Close()
		i.logger.Debug().Msgf("end copying %s", job.urn)
		done <- resultErr
		i.logger.Debug().Msgf("end copy func %s", job.urn)
	}()
	i.logger.Debug().Msgf("start indexing %s", job.urn)
	result, err := i.indexer.Stream(pr, []string{name}, []string{"siegfried", "ffprobe", "tika", "identify", "xml"})
	if err != nil {
		return errors.Wrapf(err, "cannot index %s", job.urn)
	}
	i.logger.Debug().Msgf("end indexing %s", job.urn)
	if err := <-done; err != nil {
		return errors.Wrapf(err, "cannot copy %s", job.urn)
	}
	i.logger.Debug().Msgf("%s/%s: %s/%s", job.collection.Name, job.signature, result.Type, result.Subtype)
	return nil
}

func (i *Ingester) Start() error {
	go func() {
		for {
			for {
				item, err := i.dbClient.GetIngestItem(context.Background(), &emptypb.Empty{})
				if err != nil {
					if s, ok := status.FromError(err); ok {
						if s.Code() == codes.NotFound {
							i.logger.Info().Msg("no ingest item available")
						} else {
							i.logger.Error().Err(err).Msg("cannot get ingest item")
						}
					} else {
						i.logger.Error().Err(err).Msg("cannot get ingest item")
					}
					break // on all errors we break
				}
				if item.GetIdentifier() == nil {
					i.logger.Error().Msg("empty identifier")
					break
				}

				coll := item.GetCollection()
				stor := coll.GetStorage()
				job := &JobStruct{
					signature:  item.GetIdentifier().GetSignature(),
					urn:        item.GetUrn(),
					ingestType: IngestType(item.GetIngestType()),
					collection: &collectionStruct{
						Name: coll.GetName(),
						Storage: &storageStruct{
							Name:       stor.GetName(),
							Filebase:   stor.GetFilebase(),
							Datadir:    stor.GetDatadir(),
							Subitemdir: stor.GetSubitemdir(),
							Tempdir:    stor.GetTempdir(),
						},
					},
				}
				i.jobChan <- job
				i.logger.Debug().Msgf("ingest item %s/%s", job.collection.Name, job.signature)
				// check for end without blocking
				select {
				case <-i.end:
					close(i.end)
					return
				default:
				}
			}
			select {
			case <-i.end:
				close(i.end)
				return
			case <-time.After(i.ingestWait):
			}
		}
	}()
	return nil
}

func (i *Ingester) Close() error {
	i.end <- true
	return i.worker.Close()
}
