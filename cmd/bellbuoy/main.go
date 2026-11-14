// Command bellbuoy turns failures, paused items and approved edits into one
// piece of work in the repository that owns the fix.
//
// It has two modes. serve leases steps from Muster over HTTP and works on
// them until it is told to stop. replay runs a file of jobs through the whole
// worker against in-memory stand-ins and prints what they hold afterwards.
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
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/ownership"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
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
	Contract = muster.Contract

	// WebhookEnv names the environment variable that holds the address of
	// the owner's webhook. The address is treated as a credential.
	WebhookEnv = "BELLBUOY_WEBHOOK_URL"

	// Environment variables accepted by serve (and zero-argument in-cluster startup).
	MusterEnv          = "MUSTER_URL"
	PlainforgeEnv      = "PLAINFORGE_URL"
	PlainforgeTokenEnv = "PLAINFORGE_TOKEN_FILE"
	TokenFileEnv       = "BELLBUOY_TOKEN_FILE"
	OwnersEnv          = "BELLBUOY_OWNERS"
	SpoolEnv           = "BELLBUOY_SPOOL_DIR"
	ListenEnv          = "BELLBUOY_LISTEN"

	defaultClusterMuster     = "http://muster.clearedstack.svc.cluster.local:8080"
	defaultClusterPlainforge = "http://plainforge.clearedstack.svc.cluster.local:8080"
	defaultClusterListen     = ":8080"

	// softLimit keeps the Go heap well inside the 30 MiB budget.
	softLimit = 24 << 20

	maxLine = work.MaxPayload + 4096
)

const usage = `bellbuoy serve -muster ADDRESS -owners FILE [-plainforge ADDRESS]
               [-plainforge-token-file FILE] [-listen ADDR]
               [-spool DIR] [-ledger FILE] [-token-file FILE]
               [-idle TIME] [-heartbeat TIME]
    Lease steps from the Muster at ADDRESS and work on them until told to
    stop. Each result is written to the spool before Muster is told, and
    removed only when Muster has acknowledged it.

    When -plainforge is set, tasks, proposed changes and mirror sync calls
    go to Plainforge over its Forge HTTP Contract v1 (/v1/repos/{repo}/...).
    Otherwise tasks and proposed changes are kept in the local ledger file
    (default: tasks.ledger in the spool, where the default spool is
    ` + spool.DefaultDir + `).

    If ` + WebhookEnv + ` is set, owner alerts are posted there. If it is
    not, no alerts are sent.

bellbuoy replay -jobs FILE -owners FILE [-spool DIR]
    Run the jobs in FILE (one JSON job per line) through the worker against
    in-memory stand-ins for Muster and Plainforge, then print what the
    stand-ins hold. The same input always prints the same output.

    If ` + WebhookEnv + ` is set, owner alerts are posted there. If it is
    not, they are kept and printed with the rest.

bellbuoy version
    Print the name, version and contract version.
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
		if getenv(MusterEnv) != "" || getenv("KUBERNETES_SERVICE_HOST") != "" {
			return serveClusterDefaults(ctx, getenv, stderr)
		}
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "%s %s contract %s\n", Name, Version, Contract)
		return 0
	case "replay":
		return replay(ctx, args[1:], getenv, stdout, stderr)
	case "serve":
		return serve(ctx, args[1:], getenv, stderr)
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

	deps := tools.Deps{Forge: set.Forge, Owners: set.Owners, Sender: set.Alerts, Stream: set.Queue}
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

type serveConfig struct {
	musterAddr      string
	ownersPath      string
	useDefaultMap   bool
	plainforgeAddr  string
	plainforgeToken string
	spoolDir        string
	ledgerPath      string
	tokenFile       string
	listenAddr      string
	idle            time.Duration
	heartbeat       time.Duration
}

// serveClusterDefaults runs serve when started in-cluster without CLI flags
// (for example when reconciled directly from app.yaml by Shipkeeper).
func serveClusterDefaults(ctx context.Context, getenv func(string) string, stderr io.Writer) int {
	ownersPath := getenv(OwnersEnv)
	useDefault := false
	if ownersPath == "" {
		if _, err := os.Stat(ownership.DefaultPath); err == nil {
			ownersPath = ownership.DefaultPath
		} else {
			useDefault = true
		}
	}
	cfg := serveConfig{
		musterAddr:      firstNonEmpty(getenv(MusterEnv), defaultClusterMuster),
		ownersPath:      ownersPath,
		useDefaultMap:   useDefault,
		plainforgeAddr:  firstNonEmpty(getenv(PlainforgeEnv), defaultClusterPlainforge),
		plainforgeToken: getenv(PlainforgeTokenEnv),
		spoolDir:        firstNonEmpty(getenv(SpoolEnv), spool.DefaultDir),
		tokenFile:       firstNonEmpty(getenv(TokenFileEnv), muster.DefaultTokenFile),
		listenAddr:      firstNonEmpty(getenv(ListenEnv), defaultClusterListen),
		idle:            worker.DefaultIdle,
		heartbeat:       worker.DefaultHeartbeat,
	}
	return runServe(ctx, cfg, getenv, stderr)
}

// serve leases steps from a real Muster until ctx is done.
//
// Only the sub-tools that have somewhere real to deliver to are run.
// internal-git always runs, against Plainforge over Forge HTTP Contract v1
// when -plainforge is set or against the local task ledger otherwise.
// github-mirror runs when -plainforge is set so Plainforge's mirror endpoint
// is notified on repository changes. webhook-alert runs when a webhook is
// configured. portal-sse does not run: Muster's contract v1 emits proposal
// state events directly from Muster's own /v1/feed.
func serve(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	address := fs.String("muster", getenv(MusterEnv), "address of Muster, such as http://host:port")
	ownersPath := fs.String("owners", getenv(OwnersEnv), "ownership_map.json")
	plainforgeAddr := fs.String("plainforge", getenv(PlainforgeEnv), "address of Plainforge over Forge HTTP Contract v1")
	plainforgeToken := fs.String("plainforge-token-file", getenv(PlainforgeTokenEnv), "optional bearer token file for Plainforge")
	spoolDir := fs.String("spool", firstNonEmpty(getenv(SpoolEnv), spool.DefaultDir), "spool directory")
	ledgerPath := fs.String("ledger", "", "task ledger file when -plainforge is not set")
	tokenFile := fs.String("token-file", firstNonEmpty(getenv(TokenFileEnv), muster.DefaultTokenFile), "ServiceAccount token file")
	listenAddr := fs.String("listen", getenv(ListenEnv), "optional TCP address for /healthz and readiness probes, such as :8080")
	idle := fs.Duration("idle", worker.DefaultIdle, "how long to wait when Muster has nothing to lease")
	heartbeat := fs.Duration("heartbeat", worker.DefaultHeartbeat, "how often a held lease is kept alive")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *address == "" || *ownersPath == "" || fs.NArg() != 0 || *idle <= 0 || *heartbeat <= 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cfg := serveConfig{
		musterAddr:      *address,
		ownersPath:      *ownersPath,
		plainforgeAddr:  *plainforgeAddr,
		plainforgeToken: *plainforgeToken,
		spoolDir:        *spoolDir,
		ledgerPath:      *ledgerPath,
		tokenFile:       *tokenFile,
		listenAddr:      *listenAddr,
		idle:            *idle,
		heartbeat:       *heartbeat,
	}
	return runServe(ctx, cfg, getenv, stderr)
}

func runServe(ctx context.Context, cfg serveConfig, getenv func(string) string, stderr io.Writer) int {
	if cfg.plainforgeAddr != "" && cfg.ledgerPath != "" {
		return fail(stderr, codes.Broken("plainforge address and task ledger cannot both be set"))
	}

	queue, err := muster.NewClient(cfg.musterAddr, cfg.tokenFile, 0)
	if err != nil {
		return fail(stderr, err)
	}
	var owners *ownership.Map
	if cfg.useDefaultMap {
		owners, err = ownership.Default()
	} else {
		owners, err = ownership.Load(cfg.ownersPath)
	}
	if err != nil {
		return fail(stderr, err)
	}
	sp, err := spool.Open(cfg.spoolDir)
	if err != nil {
		return fail(stderr, err)
	}
	var forge plainforge.Forge
	if cfg.plainforgeAddr != "" {
		forge, err = plainforge.NewClient(cfg.plainforgeAddr, cfg.plainforgeToken, 0)
		if err != nil {
			return fail(stderr, err)
		}
	} else {
		ledgerPath := cfg.ledgerPath
		if ledgerPath == "" {
			ledgerPath = filepath.Join(cfg.spoolDir, "tasks.ledger")
		}
		forge, err = plainforge.OpenLedger(ledgerPath)
		if err != nil {
			return fail(stderr, err)
		}
	}

	deps := tools.Deps{Forge: forge, Owners: owners}
	names := []string{"internal-git"}
	if cfg.plainforgeAddr != "" {
		names = append(names, "github-mirror")
	}
	if webhook := getenv(WebhookEnv); webhook != "" {
		sender, err := webhookalert.NewHTTPSender(webhook, 0)
		if err != nil {
			return fail(stderr, err)
		}
		deps.Sender = sender
		names = append(names, "webhook-alert")
	}

	_, stopProbe, err := startProbe(cfg.listenAddr)
	if err != nil {
		return fail(stderr, err)
	}
	defer stopProbe()

	w := &worker.Worker{
		Queue:     queue,
		Tools:     tools.Only(tools.All(deps), names...),
		Spool:     sp,
		Log:       func(line string) { fmt.Fprintln(stderr, line) },
		Idle:      cfg.idle,
		Heartbeat: cfg.heartbeat,
	}
	fmt.Fprintf(stderr, "%s serve contract=%s tools=%s staged=%d unreadable=%d\n",
		Name, Contract, strings.Join(names, ","), sp.Len(), sp.Skipped())
	w.Run(ctx)
	fmt.Fprintf(stderr, "%s stopped staged=%d\n", Name, sp.Len())
	return 0
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
