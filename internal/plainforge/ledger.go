package plainforge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/ClearedStack/bellbuoy/internal/codes"
)

// maxLedger is the largest ledger file that will be read, in bytes.
const maxLedger = 8 << 20

// Ledger is a stand-in for Plainforge that keeps its tasks and proposed
// changes in one file, so they are still there after a restart.
//
// It exists because Plainforge does not yet have an interface for tasks.
// Until it does, a task filed here is recorded in the ledger and reaches no
// repository. The file is replaced whole on every change, by writing a new
// file and renaming it over the old one. A change that cannot be written is
// undone in memory and reported as E_TIMEOUT, so it is tried again and not
// counted twice.
type Ledger struct {
	*Memory
	mu   sync.Mutex
	path string
}

var _ Forge = (*Ledger)(nil)

// OpenLedger reads the ledger at path. A file that is not there yet is an
// empty ledger. A file that cannot be understood is refused: starting with
// an empty ledger would open every task a second time.
func OpenLedger(path string) (*Ledger, error) {
	l := &Ledger{Memory: NewMemory(), path: path}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, codes.Broken("task ledger cannot be read")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxLedger+1))
	if err != nil || len(data) > maxLedger {
		return nil, codes.Broken("task ledger cannot be read")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s snapshot
	if err := dec.Decode(&s); err != nil {
		return nil, codes.Broken("task ledger is not in the expected form")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, codes.Broken("task ledger is not in the expected form")
	}
	l.Memory.restore(s)
	return l, nil
}

// change runs one change to the ledger and writes the file, or undoes the
// change if the file cannot be written.
func (l *Ledger) change(do func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	before := l.Memory.snapshot()
	if err := do(); err != nil {
		return err
	}
	if err := l.save(); err != nil {
		l.Memory.restore(before)
		return err
	}
	return nil
}

func (l *Ledger) save() error {
	data, err := json.Marshal(l.Memory.snapshot())
	if err != nil {
		return codes.TimedOut("task ledger could not be written")
	}
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return codes.TimedOut("task ledger could not be written")
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return codes.TimedOut("task ledger could not be written")
	}
	if err := os.Rename(tmp, l.path); err != nil {
		_ = os.Remove(tmp)
		return codes.TimedOut("task ledger could not be written")
	}
	if d, err := os.Open(filepath.Dir(l.path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (l *Ledger) OpenTask(ctx context.Context, repo string, draft TaskDraft) (task Task, err error) {
	err = l.change(func() error {
		task, err = l.Memory.OpenTask(ctx, repo, draft)
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return task, nil
}

func (l *Ledger) AddOccurrence(ctx context.Context, repo, taskID string) (task Task, err error) {
	err = l.change(func() error {
		task, err = l.Memory.AddOccurrence(ctx, repo, taskID)
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return task, nil
}

func (l *Ledger) ProposeChange(ctx context.Context, change Change) (created bool, err error) {
	err = l.change(func() error {
		created, err = l.Memory.ProposeChange(ctx, change)
		return err
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// SyncMirror is refused. A ledger has no mirror, and saying a mirror was
// brought up to date when nothing was sent would be a false report.
func (l *Ledger) SyncMirror(context.Context, string, []string) error {
	return codes.Unfamiliar("mirroring needs the real Plainforge")
}
