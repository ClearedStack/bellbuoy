// Package spool keeps unfinished work on disk, under /var/spool/cs in the
// cluster, so that it survives a restart of Bellbuoy or of Muster.
//
// A step is written here when a call outside timed out and has to be tried
// again, and always before Muster is told how the step ended. The record is
// removed only once Muster has acknowledged the answer. Each step has one
// file, replaced whole each time by writing a new file and renaming it over
// the old one, so a file is never seen half written.
//
// Records are named by the step (its kind and the item or proposal it
// belongs to), not by the lease, because the same step can be handed out
// again under a new lease.
package spool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

const (
	// DefaultDir is where the spool lives in the cluster.
	DefaultDir = "/var/spool/cs"

	firstDelay = time.Second
	maxDelay   = 5 * time.Minute
	maxFile    = 2 << 20
	suffix     = ".json"
	tmpPrefix  = "tmp-"
)

// Backoff returns how long to wait before trying again after the given
// number of attempts in a row without progress: one second, doubling each
// time, to at most five minutes. There is no random part, so the same
// history always gives the same schedule.
func Backoff(attempts int) time.Duration {
	d := firstDelay
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= maxDelay {
			return maxDelay
		}
	}
	return d
}

// Fault is the first rule failure met while working on a job.
type Fault struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// Record is everything needed to carry on with one job.
type Record struct {
	// Seq orders records by when they were first staged.
	Seq uint64 `json:"seq"`
	// Job is the job as leased. Its lease token is replaced if Muster hands
	// the step out again.
	Job work.Job `json:"job"`
	// Done names the sub-tools that have finished with this job.
	Done []string `json:"done,omitempty"`
	// State is what those sub-tools left behind.
	State work.State `json:"state"`
	// Fault is the first rule failure, if any.
	Fault *Fault `json:"fault,omitempty"`
	// Finished is true once every sub-tool is done and only the answer to
	// Muster is outstanding.
	Finished bool `json:"finished"`
	// Attempts counts tries in a row without progress.
	Attempts int `json:"attempts"`
	// RetryAt is the earliest time to try again, in Unix seconds.
	RetryAt int64 `json:"retry_at"`
}

type entry struct {
	seq      uint64
	retryAt  int64
	finished bool
	lease    string
}

func entryOf(rec *Record) entry {
	return entry{seq: rec.Seq, retryAt: rec.RetryAt, finished: rec.Finished, lease: rec.Job.Lease}
}

// Spool is a directory of records. It is safe for use by several goroutines.
type Spool struct {
	mu      sync.Mutex
	dir     string
	index   map[string]entry
	nextSeq uint64
	skipped int
}

// Open reads the directory, creating it if need be. A file that cannot be
// understood is left where it is and counted in Skipped: it is neither
// guessed at nor deleted.
func Open(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, codes.Broken("spool directory cannot be used")
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, codes.Broken("spool directory cannot be used")
	}
	s := &Spool{dir: dir, index: map[string]entry{}, nextSeq: 1}
	for _, de := range names {
		name := de.Name()
		if de.IsDir() {
			continue
		}
		if strings.HasPrefix(name, tmpPrefix) {
			// A write that never finished. The record it was replacing is
			// still whole.
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		rec, err := read(filepath.Join(dir, name))
		if err != nil || fileName(rec.Job.Key()) != name {
			s.skipped++
			continue
		}
		s.index[rec.Job.Key()] = entryOf(rec)
		if rec.Seq >= s.nextSeq {
			s.nextSeq = rec.Seq + 1
		}
	}
	return s, nil
}

func fileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16]) + suffix
}

func read(path string) (*Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFile {
		return nil, codes.Broken("spool record is too large")
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if rec.Job.ID == "" || rec.Seq == 0 {
		return nil, codes.Broken("spool record is incomplete")
	}
	return &rec, nil
}

// Skipped returns how many files Open could not understand.
func (s *Spool) Skipped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skipped
}

// Len returns how many steps are staged.
func (s *Spool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.index)
}

// Has reports whether a step is staged. key is the job's Key.
func (s *Spool) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.index[key]
	return ok
}

// Put writes a record, replacing any earlier one for the same step.
func (s *Spool) Put(rec *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Seq == 0 {
		rec.Seq = s.nextSeq
		s.nextSeq++
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return codes.Broken("spool record cannot be written")
	}
	name := fileName(rec.Job.Key())
	tmp := filepath.Join(s.dir, tmpPrefix+name)
	if err := writeSynced(tmp, data); err != nil {
		_ = os.Remove(tmp)
		return codes.Broken("spool record cannot be written")
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, name)); err != nil {
		_ = os.Remove(tmp)
		return codes.Broken("spool record cannot be written")
	}
	syncDir(s.dir)
	s.index[rec.Job.Key()] = entryOf(rec)
	return nil
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// Get reads a staged record back.
func (s *Spool) Get(key string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[key]; !ok {
		return nil, codes.Broken("spool has no record for this job")
	}
	rec, err := read(filepath.Join(s.dir, fileName(key)))
	if err != nil || rec.Job.Key() != key {
		return nil, codes.Broken("spool record cannot be read back")
	}
	return rec, nil
}

// Delete removes a step's record. Deleting one that is not there is fine.
func (s *Spool) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[key]; !ok {
		return nil
	}
	if err := os.Remove(filepath.Join(s.dir, fileName(key))); err != nil && !os.IsNotExist(err) {
		return codes.Broken("spool record cannot be removed")
	}
	syncDir(s.dir)
	delete(s.index, key)
	return nil
}

// Due returns the keys of the steps whose retry time has come, oldest first.
func (s *Spool) Due(now time.Time) []string {
	return s.list(func(e entry) bool { return e.retryAt <= now.Unix() })
}

// Leases returns the lease tokens of the staged steps that still have
// sub-tools to run, oldest first. Those leases have to be kept alive.
func (s *Spool) Leases() []string {
	keys := s.list(func(e entry) bool { return !e.finished && e.lease != "" })
	s.mu.Lock()
	defer s.mu.Unlock()
	leases := make([]string, 0, len(keys))
	for _, key := range keys {
		leases = append(leases, s.index[key].lease)
	}
	return leases
}

func (s *Spool) list(keep func(entry) bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.index))
	for key, e := range s.index {
		if keep(e) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return s.index[keys[i]].seq < s.index[keys[j]].seq })
	return keys
}
