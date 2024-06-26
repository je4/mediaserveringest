package ingest

import (
	"context"
	"emperror.dev/errors"
	"encoding/json"
	"github.com/je4/filesystem/v3/pkg/writefs"
	genericproto "github.com/je4/genericproto/v2/pkg/generic/proto"
	"github.com/je4/indexer/v3/pkg/indexer"
	mediaserverproto "github.com/je4/mediaserverproto/v2/pkg/mediaserver/proto"
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

func NewIngester(indexer *indexer.ActionDispatcher, dbClient mediaserverproto.DatabaseClient, vfs fs.FS, concurrentWorkers int, ingestTimeout time.Duration, ingestWait time.Duration, logger zLogger.ZLogger) (*Ingester, error) {
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
	dbClient      mediaserverproto.DatabaseClient
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
	var objectType = "file"
	var itemMetadata = &mediaserverproto.ItemMetadata{
		Objecttype: &objectType,
	}
	var cacheMetadata = &mediaserverproto.CacheMetadata{
		Action: "item",
		Params: "",
		Path:   job.urn,
	}
	var fullMetadata string
	result, err := i.indexer.Stream(pr, []string{name}, []string{"siegfried", "ffprobe", "tika", "identify", "xml", "checksum"})
	var resultErrs []error
	if err != nil {
		i.logger.Error().Err(err).Msgf("cannot index %s", job.urn)
		resultErrs = append(resultErrs, err)
	} else {
		itemMetadata.Type = &result.Type
		result.Subtype = strings.ToLower(result.Subtype)
		itemMetadata.Subtype = &result.Subtype
		itemMetadata.Mimetype = &result.Mimetype
		checksum, _ := result.Checksum["sha512"]
		itemMetadata.Sha512 = &checksum
		if metaBytes, err := json.Marshal(result.Metadata); err == nil {
			metaBytes, err = json.Marshal(struct {
				Path      string            `json:"path"`
				Errors    map[string]string `json:"errors,omitempty"`
				Mimetype  string            `json:"mimetype"`
				Mimetypes []string          `json:"mimetypes"`
				Pronom    string            `json:"pronom"`
				Pronoms   []string          `json:"pronoms"`
				Width     uint              `json:"width,omitempty"`
				Height    uint              `json:"height,omitempty"`
				Duration  uint              `json:"duration,omitempty"`
				Type      string            `json:"type"`
				Subtype   string            `json:"subtype"`
				Metadata  json.RawMessage   `json:"metadata"`
			}{
				Path:      job.urn,
				Errors:    result.Errors,
				Mimetype:  result.Mimetype,
				Mimetypes: result.Mimetypes,
				Pronom:    result.Pronom,
				Pronoms:   result.Pronoms,
				Width:     result.Width,
				Height:    result.Height,
				Duration:  result.Duration,
				Type:      result.Type,
				Subtype:   result.Subtype,
				Metadata:  metaBytes,
			})
			if err != nil {
				resultErrs = append(resultErrs, err)
			}
			fullMetadata = string(metaBytes)
		}

		cacheMetadata.Width = int64(result.Width)
		cacheMetadata.Height = int64(result.Height)
		cacheMetadata.Duration = int64(result.Duration)
		cacheMetadata.Size = int64(result.Size)
		cacheMetadata.MimeType = result.Mimetype
		cacheMetadata.Path = cachePath
		if job.ingestType != IngestType_KEEP {
			cacheMetadata.Storage = &mediaserverproto.Storage{
				Name:       job.collection.Storage.Name,
				Filebase:   job.collection.Storage.Filebase,
				Datadir:    job.collection.Storage.Datadir,
				Subitemdir: job.collection.Storage.Subitemdir,
				Tempdir:    job.collection.Storage.Tempdir,
			}

		}

	}
	i.logger.Debug().Msgf("end indexing %s", job.urn)
	if err := <-done; err != nil {
		resultErrs = append(resultErrs, err)
		i.logger.Error().Err(err).Msgf("cannot copy %s", job.urn)
	}
	i.logger.Debug().Msgf("%s/%s: %s/%s", job.collection.Name, job.signature, result.Type, result.Subtype)
	var status = "ok"
	var itemError = ""
	if len(resultErrs) > 0 {
		status = "error"
		itemError = errors.Combine(resultErrs...).Error()
	}
	ingestMetadata := &mediaserverproto.IngestMetadata{
		Item: &mediaserverproto.ItemIdentifier{
			Collection: job.collection.Name,
			Signature:  job.signature,
		},
		Status:        status,
		ItemMetadata:  itemMetadata,
		CacheMetadata: cacheMetadata,
		FullMetadata:  fullMetadata,
	}
	if itemError != "" {
		ingestMetadata.Error = &itemError
	}

	if resp, err := i.dbClient.SetIngestItem(context.Background(), ingestMetadata); err != nil {
		return errors.Wrapf(err, "cannot set ingest item %s/%s", job.collection.Name, job.signature)
	} else {
		if resp.GetStatus() != genericproto.ResultStatus_OK {
			return errors.Errorf("cannot set ingest item %s/%s: %s", job.collection.Name, job.signature, resp.GetMessage())
		} else {
			i.logger.Debug().Msgf("set ingest item %s/%s: %s", job.collection.Name, job.signature, resp.GetMessage())
		}
	}
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
