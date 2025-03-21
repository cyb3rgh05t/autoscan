package bernard

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	lowe "github.com/l3uddz/bernard"
	ds "github.com/l3uddz/bernard/datastore"
	"github.com/l3uddz/bernard/datastore/sqlite"
	"github.com/m-rots/stubbs"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"github.com/cyb3rgh05t/autoscan"
)

const (
	maxSyncRetries = 5
)

type Config struct {
	AccountPath  string             `yaml:"account"`
	CronSchedule string             `yaml:"cron"`
	Priority     int                `yaml:"priority"`
	TimeOffset   time.Duration      `yaml:"time-offset"`
	Verbosity    string             `yaml:"verbosity"`
	Rewrite      []autoscan.Rewrite `yaml:"rewrite"`
	Include      []string           `yaml:"include"`
	Exclude      []string           `yaml:"exclude"`
	Drives       []struct {
		ID         string             `yaml:"id"`
		TimeOffset time.Duration      `yaml:"time-offset"`
		Rewrite    []autoscan.Rewrite `yaml:"rewrite"`
		Include    []string           `yaml:"include"`
		Exclude    []string           `yaml:"exclude"`
	} `yaml:"drives"`
}

func New(c Config, db *sql.DB) (autoscan.Trigger, error) {
	l := autoscan.GetLogger(c.Verbosity).With().
		Str("trigger", "bernard").
		Logger()

	const scope = "https://www.googleapis.com/auth/drive.readonly"
	auth, err := stubbs.FromFile(c.AccountPath, []string{scope})
	if err != nil {
		return nil, fmt.Errorf("%v: %w", err, autoscan.ErrFatal)
	}

	store, err := sqlite.FromDB(db)
	if err != nil {
		return nil, fmt.Errorf("%v: %w", err, autoscan.ErrFatal)
	}

	limiter, err := getRateLimiter(auth.Email())
	if err != nil {
		return nil, fmt.Errorf("%v: %w", err, autoscan.ErrFatal)
	}

	bernard := lowe.New(auth, store,
		lowe.WithPreRequestHook(limiter.Wait),
		lowe.WithSafeSleep(120*time.Second))

	var drives []drive
	for _, d := range c.Drives {
		d := d

		rewriter, err := autoscan.NewRewriter(append(d.Rewrite, c.Rewrite...))
		if err != nil {
			return nil, err
		}

		filterer, err := autoscan.NewFilterer(append(d.Include, c.Include...), append(d.Exclude, c.Exclude...))
		if err != nil {
			return nil, err
		}

		scanTime := func() time.Time {
			if d.TimeOffset.Seconds() > 0 {
				return time.Now().Add(d.TimeOffset)
			}
			return time.Now().Add(c.TimeOffset)
		}

		drives = append(drives, drive{
			ID:       d.ID,
			Rewriter: rewriter,
			Allowed:  filterer,
			ScanTime: scanTime,
		})
	}

	trigger := func(callback autoscan.ProcessorFunc) {
		d := daemon{
			log:          l,
			callback:     callback,
			cronSchedule: c.CronSchedule,
			priority:     c.Priority,
			drives:       drives,
			bernard:      bernard,
			store:        &bds{store},
			limiter:      limiter,
		}

		// start job(s)
		if err := d.startAutoSync(); err != nil {
			l.Error().
				Err(err).
				Msg("Failed initialising cron jobs")
			return
		}
	}

	return trigger, nil
}

type drive struct {
	ID       string
	Rewriter autoscan.Rewriter
	Allowed  autoscan.Filterer
	ScanTime func() time.Time
}

type daemon struct {
	callback     autoscan.ProcessorFunc
	cronSchedule string
	priority     int
	drives       []drive
	bernard      *lowe.Bernard
	store        *bds
	log          zerolog.Logger
	limiter      *rateLimiter
}

type syncJob struct {
	log      zerolog.Logger
	attempts int
	errors   []error

	cron  *cron.Cron
	jobID cron.EntryID
	fn    func() error
}

func (s *syncJob) Run() {
	// increase attempt counter
	s.attempts++

	// run job
	err := s.fn()

	// handle job response
	switch {
	case err == nil:
		// job completed successfully
		s.attempts = 0
		s.errors = s.errors[:0]
		return

	case errors.Is(err, lowe.ErrInvalidCredentials), errors.Is(err, ds.ErrDataAnomaly), errors.Is(err, lowe.ErrNetwork):
		//retryable error occurred
		s.log.Trace().
			Err(err).
			Int("attempts", s.attempts).
			Msg("Retryable error occurred while syncing drive")
		s.errors = append(s.errors, err)

	case errors.Is(err, autoscan.ErrFatal):
		// fatal error occurred, we cannot recover from this safely
		s.log.Error().
			Err(err).
			Msg("Fatal error occurred while syncing drive, drive has been stopped...")

		s.cron.Remove(s.jobID)
		return

	case err != nil:
		// an un-expected/un-handled error occurred, this should be retryable with the same retry logic
		s.log.Warn().
			Err(err).
			Int("attempts", s.attempts).
			Msg("Unexpected error occurred while syncing drive")
		s.errors = append(s.errors, err)
	}

	// abort if max retries reached
	if s.attempts >= maxSyncRetries {
		s.log.Error().
			Errs("error", s.errors).
			Int("attempts", s.attempts).
			Msg("Consecutive errors occurred while syncing drive, drive has been stopped...")

		s.cron.Remove(s.jobID)
	}
}

func newSyncJob(c *cron.Cron, log zerolog.Logger, job func() error) *syncJob {
	return &syncJob{
		log:      log,
		attempts: 0,
		errors:   make([]error, 0),
		cron:     c,
		fn:       job,
	}
}

func (d daemon) startAutoSync() error {
	c := cron.New()

	for _, drive := range d.drives {
		drive := drive
		fullSync := false
		l := d.withDriveLog(drive.ID)

		// full sync required?
		_, err := d.store.PageToken(drive.ID)
		switch {
		case errors.Is(err, ds.ErrFullSync):
			fullSync = true
		case err != nil:
			return fmt.Errorf("%v: determining if full sync required: %v: %w",
				drive.ID, err, autoscan.ErrFatal)
		}

		// create job
		job := newSyncJob(c, l, func() error {
			// acquire lock
			if err := d.limiter.Acquire(1); err != nil {
				return fmt.Errorf("%v: acquiring sync semaphore: %v: %w",
					drive.ID, err, autoscan.ErrFatal)
			}
			defer d.limiter.Release(1)

			// full sync
			if fullSync {
				l.Info().Msg("Starting full sync")
				start := time.Now()

				if err := d.bernard.FullSync(drive.ID); err != nil {
					return fmt.Errorf("%v: performing full sync: %w", drive.ID, err)
				}

				l.Info().Msgf("Finished full sync in %s", time.Since(start))
				fullSync = false
				return nil
			}

			// create partial sync
			dh, diff := d.store.NewDifferencesHook()
			ph := NewPostProcessBernardDiff(drive.ID, d.store, diff)
			ch, paths := NewPathsHook(drive.ID, d.store, diff)

			l.Trace().Msg("Running partial sync")
			start := time.Now()

			// do partial sync
			err := d.bernard.PartialSync(drive.ID, dh, ph, ch)
			if err != nil {
				return fmt.Errorf("%v: performing partial sync: %w", drive.ID, err)
			}

			l.Trace().
				Int("new", len(paths.NewFolders)).
				Int("old", len(paths.OldFolders)).
				Msgf("Partial sync finished in %s", time.Since(start))

			// translate paths to scan task
			task := d.getScanTask(&(drive), paths)

			// move scans to processor
			if len(task.scans) > 0 {
				l.Trace().
					Interface("scans", task.scans).
					Msg("Scans moving to processor")

				err := d.callback(task.scans...)
				if err != nil {
					return fmt.Errorf("%v: moving scans to processor: %v: %w",
						drive.ID, err, autoscan.ErrFatal)
				}

				l.Info().
					Int("added", task.added).
					Int("removed", task.removed).
					Msg("Scan moved to processor")
			}

			return nil
		})

		id, err := c.AddJob(d.cronSchedule, cron.NewChain(cron.SkipIfStillRunning(cron.DiscardLogger)).Then(job))
		if err != nil {
			return fmt.Errorf("%v: creating auto sync job for drive: %w", drive.ID, err)
		}

		job.jobID = id
	}

	c.Start()
	return nil
}

type scanTask struct {
	scans   []autoscan.Scan
	added   int
	removed int
}

func (d daemon) getScanTask(drive *drive, paths *Paths) *scanTask {
	pathMap := make(map[string]int)
	task := &scanTask{
		scans:   make([]autoscan.Scan, 0),
		added:   0,
		removed: 0,
	}

	for _, p := range paths.NewFolders {
		// rewrite path
		rewritten := drive.Rewriter(p)

		// check if path already seen
		if _, ok := pathMap[rewritten]; ok {
			// already a scan task present
			continue
		} else {
			pathMap[rewritten] = 1
		}

		// is this path allowed?
		if !drive.Allowed(rewritten) {
			continue
		}

		// add scan task
		task.scans = append(task.scans, autoscan.Scan{
			Folder:   filepath.Clean(rewritten),
			Priority: d.priority,
			Time:     drive.ScanTime(),
		})

		task.added++
	}

	for _, p := range paths.OldFolders {
		// rewrite path
		rewritten := drive.Rewriter(p)

		// check if path already seen
		if _, ok := pathMap[rewritten]; ok {
			// already a scan task present
			continue
		} else {
			pathMap[rewritten] = 1
		}

		// is this path allowed?
		if !drive.Allowed(rewritten) {
			continue
		}

		// add scan task
		task.scans = append(task.scans, autoscan.Scan{
			Folder:   filepath.Clean(rewritten),
			Priority: d.priority,
			Time:     drive.ScanTime(),
		})

		task.removed++
	}

	return task
}

func (d daemon) withDriveLog(driveID string) zerolog.Logger {
	drive, err := d.store.GetDrive(driveID)
	if err != nil {
		return d.log.With().Str("drive_id", driveID).Logger()
	}

	return d.log.With().Str("drive_id", driveID).Str("drive_name", drive.Name).Logger()
}
