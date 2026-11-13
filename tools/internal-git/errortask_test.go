package internalgit_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/clearedstack/bellbuoy/internal/errcode"
	"github.com/clearedstack/bellbuoy/internal/plainforge"
	internalgit "github.com/clearedstack/bellbuoy/tools/internal-git"
)

const sample = `{"kind":"error","worker":"keelworks","code":"E_TIMEOUT","pattern":"build-step-slow"}`

func TestTwoHundredAtOnceBecomeOneTask(t *testing.T) {
	store := plainforge.NewMemory()
	router := internalgit.NewRouter(store)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			router.Handle(context.Background(), []byte(sample))
		}()
	}
	wg.Wait()
	tasks := store.All()
	if len(tasks) != 1 || tasks[0].Occurrences != 200 {
		t.Fatalf("tasks = %+v, want one task with 200 occurrences", tasks)
	}
}

func TestClosedTaskIsNotAddedTo(t *testing.T) {
	ctx := context.Background()
	store := plainforge.NewMemory()
	router := internalgit.NewRouter(store)

	router.Handle(ctx, []byte(sample))
	router.Handle(ctx, []byte(sample))
	if !store.Close(1) {
		t.Fatal("could not close task 1")
	}
	o := router.Handle(ctx, []byte(sample))
	if o.Action != internalgit.ActionOpened || o.Task != 2 || o.Occurrences != 1 {
		t.Fatalf("after close got %+v, want a new task 2 with 1 occurrence", o)
	}
	o = router.Handle(ctx, []byte(sample))
	if o.Action != internalgit.ActionAdded || o.Task != 2 || o.Occurrences != 2 {
		t.Fatalf("got %+v, want added to task 2", o)
	}
	tasks := store.All()
	if len(tasks) != 2 || tasks[0].Open || tasks[0].Occurrences != 2 || tasks[0].Fingerprint != tasks[1].Fingerprint {
		t.Fatalf("tasks = %+v", tasks)
	}
}

func TestTaskTextIsExact(t *testing.T) {
	store := plainforge.NewMemory()
	internalgit.NewRouter(store).Handle(context.Background(), []byte(sample))
	got := store.All()[0]
	wantTitle := "keelworks: E_TIMEOUT build-step-slow"
	wantBody := "Worker: keelworks\nError code: E_TIMEOUT\nFailure pattern: build-step-slow\nFingerprint: " + got.Fingerprint + "\n"
	if got.Title != wantTitle || got.Body != wantBody {
		t.Errorf("title %q body %q", got.Title, got.Body)
	}
}

func TestStoreFailureWritesNothingAndLeaksNothing(t *testing.T) {
	secret := errors.New("dial tcp 10.0.0.7:3000 on node-a.example.internal: token hunter2 rejected")
	cases := []struct {
		name   string
		err    error
		action string
		code   errcode.Code
	}{
		{"timeout", plainforge.ErrTimeout, internalgit.ActionNotDelivered, errcode.Timeout},
		{"deadline", context.DeadlineExceeded, internalgit.ActionNotDelivered, errcode.Timeout},
		{"bad response", plainforge.ErrBadResponse, internalgit.ActionRefused, errcode.Parse},
		{"anything else", secret, internalgit.ActionRefused, errcode.Parse},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			store := plainforge.NewMemory()
			router := internalgit.NewRouter(store)
			router.Handle(ctx, []byte(sample))

			// Fail the lookup, then on the next item fail the write.
			for i := 0; i < 2; i++ {
				if i == 1 {
					// Let FindOpen through, fail AddOccurrence.
					store.FailNext(nil)
				}
				store.FailNext(c.err)
				o := router.Handle(ctx, []byte(sample))
				if o.Action != c.action || o.Code != c.code || o.Task != 0 {
					t.Fatalf("got %+v, want %s %s", o, c.action, c.code)
				}
				for _, bad := range []string{"10.0.0.7", "node-a", "hunter2", "dial"} {
					if strings.Contains(string(o.Reason), bad) {
						t.Fatalf("reason %q leaks %q", o.Reason, bad)
					}
				}
			}
			tasks := store.All()
			if len(tasks) != 1 || tasks[0].Occurrences != 1 {
				t.Fatalf("store changed by a failed call: %+v", tasks)
			}
		})
	}
}

func TestExpiredContextIsATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := plainforge.NewMemory()
	o := internalgit.NewRouter(store).Handle(ctx, []byte(sample))
	if o.Action != internalgit.ActionNotDelivered || o.Code != errcode.Timeout {
		t.Fatalf("got %+v", o)
	}
	if len(store.All()) != 0 {
		t.Fatal("a task was opened")
	}
}

func TestOversizedItemIsRefused(t *testing.T) {
	big := `{"kind":"error","worker":"keelworks","code":"E_TIMEOUT","pattern":"` + strings.Repeat("a", internalgit.MaxItemBytes) + `"}`
	o := internalgit.NewRouter(plainforge.NewMemory()).Handle(context.Background(), []byte(big))
	if o.Action != internalgit.ActionRefused || o.Code != errcode.Parse {
		t.Fatalf("got %+v", o)
	}
}

// Nothing from a refused or paused item may be repeated in its outcome.
func TestOutcomeNeverEchoesInput(t *testing.T) {
	items := []string{
		`{"kind":"error","worker":"keelworks","code":"E_PARSE","pattern":"dial 10.0.0.7:5432 on node-a.example.internal refused"}`,
		`{"kind":"error","worker":"node-a.example.internal","code":"E_PARSE","pattern":"x"}`,
		`{"kind":"10.0.0.7","token":"hunter2"}`,
		`{"kind":"error","worker":"keelworks","code":"E_TIMEOUT","pattern":"x","host":"node-a.example.internal"}`,
	}
	for _, it := range items {
		o := internalgit.NewRouter(plainforge.NewMemory()).Handle(context.Background(), []byte(it))
		if o.Code == "" {
			t.Fatalf("%s was accepted", it)
		}
		for _, bad := range []string{"10.0.0.7", "node-a", "hunter2", "example"} {
			if strings.Contains(string(o.Reason), bad) || o.Fingerprint != "" {
				t.Fatalf("outcome %+v repeats input", o)
			}
		}
	}
}
