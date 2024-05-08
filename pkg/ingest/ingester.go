package ingest

import (
	"context"
	"emperror.dev/errors"
	"github.com/je4/indexer/v2/pkg/indexer"
	"github.com/je4/mediaserverdb/v2/pkg/mediaserverdbproto"
	"github.com/je4/utils/v2/pkg/zLogger"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"io"
	"io/fs"
	"path/filepath"
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

func (i *Ingester) doIngest(job *JobStruct) error {
	i.logger.Debug().Msgf("ingest %s/%s", job.collection, job.signature)
	fp, err := i.vfs.Open(job.urn)
	if err != nil {
		return errors.Wrapf(err, "cannot open %s", job.urn)
	}
	defer fp.Close()
	name := filepath.Base(job.urn)
	result, err := i.indexer.Stream(fp, []string{name}, []string{"siegfried", "ffprobe", "tika", "identify", "xml"})
	if err != nil {
		return errors.Wrapf(err, "cannot index %s", job.urn)
	}
	i.logger.Debug().Msgf("%s/%s: %s/%s", job.collection, job.signature, result.Type, result.Subtype)
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
				job := &JobStruct{
					collection: item.GetIdentifier().GetCollection(),
					signature:  item.GetIdentifier().GetSignature(),
					urn:        item.GetUrn(),
				}
				i.jobChan <- job
				i.logger.Debug().Msgf("ingest item %s/%s", job.collection, job.signature)
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
