// Command bellbuoy turns failures, paused items and approved edits into one
// piece of work in the repository that owns the fix.
//
// Today it has one working mode, replay, which runs a file of jobs through
// the whole worker against in-memory stand-ins for Muster and Plainforge and
// prints what they hold afterwards. The mode that leases from a real Muster
// waits on the Muster v1 contract.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/spool"
	"github.com/ClearedStack/bellbuoy/internal/standin"
	"github.com/ClearedStack/bellbuoy/internal/work"
	"github.com/ClearedStack/bellbuoy/internal/worker"
	"github.com/ClearedStack/bellbuoy/tools"
	webhookalert "github.com/ClearedStack/bellbuoy/tools/webhook-alert"
)

const (
	// Name and Version are this component's entries in the naming registry.
	Name    = "bellbuoy"
	Version = "v0.1.0-draft"

	// Contract is the version of the platform contract this build follows.
	Contract = "v1"

	// WebhookEnv names the environment variable that holds the address of
	// the owner's webhook. The address is treated as a credential.
	WebhookEnv = "BELLBUOY_WEBHOOK_URL"

	// softLimit keeps the Go heap well inside the 30 MiB budget.
	softLimit = 24 << 20

	maxLine = work.MaxPayload + 4096
)

const usage = `bellbuoy replay -jobs FILE -owners FILE [-spool DIR]
    Run the jobs in FILE (one JSON job per line) through the worker against
    in-memory stand-ins for Muster and Plainforge, then print what the
    stand-ins hold. The same input always prints the same output.

    If ` + WebhookEnv + ` is set, owner alerts are posted there. If it is
    not, they are kept and printed with the rest.

bellbuoy version
    Print the name, version and contract version.

Leasing from a real Muster is not built yet. It waits on the Muster v1
contract, which is not in this repository.
`

func main() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(softLimit)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "%s %s contract %s\n", Name, Version, Contract)
		return 0
	case "replay":
		return replay(ctx, args[1:], getenv, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// report is what replay prints.
type report struct {
	Answers []muster.Ack `json:"answers"`
	standin.Effects
	// Held counts jobs still staged in the spool, waiting to be retried.
	Held int `json:"held"`
}

func replay(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jobsPath := fs.String("jobs", "", "file of jobs, one JSON object per line")
	ownersPath := fs.String("owners", "", "ownership_map.json")
	spoolDir := fs.String("spool", "", "spool directory (default: a temporary one, removed afterwards)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *jobsPath == "" || *ownersPath == "" || fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	set, err := standin.New(*ownersPath)
	if err != nil {
		return fail(stderr, err)
	}
	jobs, err := readJobs(*jobsPath)
	if err != nil {
		return fail(stderr, err)
	}
	for _, job := range jobs {
		// A job goes into the lane it names. One that names no real lane is
		// put in normal, where the worker will refuse it.
		lane := job.Lane
		if !work.IsLane(lane) {
			lane = work.LaneNormal
		}
		set.Queue.AddTo(lane, job)
	}

	dir := *spoolDir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "bellbuoy-spool-")
		if err != nil {
			return fail(stderr, codes.Broken("spool directory cannot be used"))
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}
	sp, err := spool.Open(dir)
	if err != nil {
		return fail(stderr, err)
	}

	deps := tools.Deps{Queue: set.Queue, Forge: set.Forge, Owners: set.Owners, Sender: set.Alerts}
	if address := getenv(WebhookEnv); address != "" {
		sender, err := webhookalert.NewHTTPSender(address, 0)
		if err != nil {
			return fail(stderr, err)
		}
		deps.Sender = sender
	}

	w := &worker.Worker{
		Queue: set.Queue,
		Tools: tools.All(deps),
		Spool: sp,
		Log:   func(line string) { fmt.Fprintln(stderr, line) },
	}
	held := w.Drain(ctx)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report{Answers: set.Queue.Acks(), Effects: set.Collect(), Held: held}); err != nil {
		return fail(stderr, codes.Broken("report could not be written out"))
	}
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		return 1
	}
	return 0
}

// readJobs reads one job per line. Blank lines are skipped. A line that is
// not a job stops the replay: the file is the input, and a broken input is
// not something to work around.
func readJobs(path string) ([]work.Job, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, codes.Broken("jobs file cannot be read")
	}
	defer f.Close()
	var jobs []work.Job
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var job work.Job
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&job); err != nil {
			return nil, codes.Broken("jobs file has a line that is not a job")
		}
		if _, err := dec.Token(); err != io.EOF {
			return nil, codes.Broken("jobs file has a line that is not a job")
		}
		jobs = append(jobs, job)
	}
	if sc.Err() != nil {
		return nil, codes.Broken("jobs file cannot be read")
	}
	return jobs, nil
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, Name+": "+codes.From(err).Error())
	return 1
}
