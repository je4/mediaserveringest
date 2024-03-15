package ingest

import (
	"emperror.dev/errors"
	"io"
	"sync"
	"time"
)

type JobStruct struct {
	collection string
	signature  string
	urn        string
}

func NewWorkerPool(num int, ingestTimeout time.Duration) io.Closer {
	wp := &workerPool{
		jobChan:       make(chan *JobStruct),
		wg:            &sync.WaitGroup{},
		ingestTimeout: ingestTimeout,
	}
	wp.Start(num)
	return wp
}

type workerPool struct {
	jobChan       chan *JobStruct
	wg            *sync.WaitGroup
	ingestTimeout time.Duration
}

func (wp *workerPool) Start(num int) {
	for i := 0; i < num; i++ {
		wp.AddWorker()
	}
}

func (wp *workerPool) Close() error {
	close(wp.jobChan)
	c := make(chan struct{})
	go func() {
		defer close(c)
		wp.wg.Wait()
	}()
	select {
	case <-c:
		return nil // completed normally
	case <-time.After(wp.ingestTimeout):
		return errors.New("timed out") // timed out
	}
}

func (wp *workerPool) AddWorker() {
	wp.wg.Add(1)
	defer wp.wg.Done()
	go func() {
		for job := range wp.jobChan {
			// process job
			_ = job
		}
	}()
}
