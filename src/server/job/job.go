package job

import (
	"dekart/src/proto"
	"dekart/src/server/uuid"
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"context"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	"github.com/rs/zerolog/log"
	"google.golang.org/api/iterator"
)

// Job of quering db, concurency safe
type Job struct {
	ID             string
	QueryID        string
	ReportID       string
	Ctx            context.Context
	cancel         context.CancelFunc
	bigqueryJob    *bigquery.Job
	Status         chan int32
	err            string
	totalRows      int64
	processedBytes int64
	resultSize     int64
	resultID       *string
	storageObj     *storage.ObjectHandle
	mutex          sync.Mutex
}

// Err of job
func (job *Job) Err() string {
	job.mutex.Lock()
	defer job.mutex.Unlock()
	return job.err
}

// GetResultSize of the job
func (job *Job) GetResultSize() int64 {
	job.mutex.Lock()
	defer job.mutex.Unlock()
	return job.resultSize
}

// GetResultID for the job; nil means results not yet saved
func (job *Job) GetResultID() *string {
	job.mutex.Lock()
	defer job.mutex.Unlock()
	return job.resultID
}

// GetTotalRows in result
func (job *Job) GetTotalRows() int64 {
	job.mutex.Lock()
	defer job.mutex.Unlock()
	return job.totalRows
}

// GetProcessedBytes in result
func (job *Job) GetProcessedBytes() int64 {
	job.mutex.Lock()
	defer job.mutex.Unlock()
	return job.processedBytes
}

var contextCancelledRe = regexp.MustCompile(`context canceled`)

func (job *Job) close(storageWriter *storage.Writer, csvWriter *csv.Writer) {
	csvWriter.Flush()
	err := storageWriter.Close()
	if err != nil {
		if err == context.Canceled {
			return
		}
		if contextCancelledRe.MatchString(err.Error()) {
			return
		}
		log.Err(err).Send()
		job.cancelWithError(err)
		return
	}
	attrs := storageWriter.Attrs()
	job.mutex.Lock()
	// TODO: use bool done
	job.resultID = &job.ID
	if attrs != nil {
		job.resultSize = attrs.Size
	}
	job.mutex.Unlock()
	job.Status <- int32(proto.Query_JOB_STATUS_DONE)
	job.cancel()
}

func (job *Job) setJobStats(queryStatus *bigquery.JobStatus, totalRows uint64) {
	job.mutex.Lock()
	defer job.mutex.Unlock()
	if queryStatus.Statistics != nil {
		job.processedBytes = queryStatus.Statistics.TotalBytesProcessed
	}
	job.totalRows = int64(totalRows)
}

func (job *Job) read(queryStatus *bigquery.JobStatus) {
	ctx := job.Ctx

	it, err := job.bigqueryJob.Read(ctx)
	if err != nil {
		log.Err(err).Send()
		job.cancelWithError(err)
		return
	}

	job.setJobStats(queryStatus, it.TotalRows)
	job.Status <- int32(queryStatus.State)

	storageWriter := job.storageObj.NewWriter(ctx)
	csvWriter := csv.NewWriter(storageWriter)
	defer job.close(storageWriter, csvWriter)

	firstLine := true

	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err == context.Canceled {
			break
		}
		if err != nil {
			log.Err(err).Send()
			job.cancelWithError(err)
			return
		}
		if firstLine {
			firstLine = false
			csvRow := make([]string, len(row), len(row))
			for i, fieldSchema := range it.Schema {
				csvRow[i] = fieldSchema.Name
				// fmt.Println(fieldSchema.Name, fieldSchema.Type)
			}
			err = csvWriter.Write(csvRow)
			if err == context.Canceled {
				break
			}
			if err != nil {
				log.Err(err).Send()
				job.cancelWithError(err)
				return
			}
		}
		csvRow := make([]string, len(row), len(row))
		for i, v := range row {
			csvRow[i] = fmt.Sprintf("%v", v)
		}
		err = csvWriter.Write(csvRow)
		if err == context.Canceled {
			break
		}
		if err != nil {
			log.Err(err).Send()
			job.cancelWithError(err)
			return
		}
	}
}

func (job *Job) cancelWithError(err error) {
	job.mutex.Lock()
	job.err = err.Error()
	job.mutex.Unlock()
	job.Status <- 0
	job.cancel()
}

func (job *Job) wait() {
	queryStatus, err := job.bigqueryJob.Wait(job.Ctx)
	if err == context.Canceled {
		return
	}
	if err != nil {
		job.cancelWithError(err)
		return
	}
	if queryStatus == nil {
		log.Fatal().Msgf("queryStatus == nil")
	}
	if err := queryStatus.Err(); err != nil {
		job.cancelWithError(err)
		return
	}
	job.read(queryStatus)
}

// Run implementation
func (job *Job) Run(queryText string, obj *storage.ObjectHandle) error {
	client, err := bigquery.NewClient(job.Ctx, os.Getenv("DEKART_BIGQUERY_PROJECT_ID"))
	if err != nil {
		job.cancel()
		return err
	}
	bigqueryJob, err := client.Query(queryText).Run(job.Ctx)
	if err != nil {
		job.cancel()
		return err
	}
	job.mutex.Lock()
	job.bigqueryJob = bigqueryJob
	job.storageObj = obj
	job.mutex.Unlock()
	job.Status <- int32(proto.Query_JOB_STATUS_RUNNING)
	go job.wait()
	return nil
}

// Store of jobs
type Store struct {
	jobs  []*Job
	mutex sync.Mutex
}

// NewStore instance
func NewStore() *Store {
	store := &Store{}
	store.jobs = make([]*Job, 0)
	return store
}

func (s *Store) removeJobWhenDone(job *Job) {
	select {
	case <-job.Ctx.Done():
		s.mutex.Lock()
		for i, j := range s.jobs {
			if job.ID == j.ID {
				// removing job from slice
				last := len(s.jobs) - 1
				s.jobs[i] = s.jobs[last]
				s.jobs = s.jobs[:last]
				break
			}
		}
		s.mutex.Unlock()
		return
	}
}

// New job on store
func (s *Store) New(reportID string, queryID string) *Job {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	job := &Job{
		ID:       uuid.GetUUID(),
		ReportID: reportID,
		QueryID:  queryID,
		Ctx:      ctx,
		cancel:   cancel,
		Status:   make(chan int32),
	}
	s.jobs = append(s.jobs, job)
	go s.removeJobWhenDone(job)
	return job
}

// Cancel job for queryID
func (s *Store) Cancel(queryID string) {
	s.mutex.Lock()
	for _, job := range s.jobs {
		if job.QueryID == queryID {
			job.Status <- int32(proto.Query_JOB_STATUS_UNSPECIFIED)
			log.Info().Msg("Canceling Job Context")
			job.cancel()
		}
	}
	s.mutex.Unlock()
}
