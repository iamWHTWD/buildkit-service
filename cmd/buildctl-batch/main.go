package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/inclusionAI/buildkit-service/pkg/dockerfilepreprocess"

	cli "github.com/urfave/cli/v2"
)

const defaultBuildkitOOMCooldown = 2 * time.Minute
const nydusV3TargetSuffix = "_nydus_v3"
const defaultBuildkitAddrRefreshInterval = 15 * time.Second
const pprofServerEnv = "BUILDCTL_BATCH_PPROF_SERVER"
const maxBuildLogBytes int64 = 1 << 20

var buildctlBatchVarPattern = regexp.MustCompile(`\$\{?(BUILDCTL_BATCH_[A-Za-z0-9_]+)\}?`)

var commandStartUnixNano atomic.Int64

var logProgressState struct {
	current atomic.Int64
	total   atomic.Int64
}

// options holds CLI flags shared across subcommands.
type options struct {
	imageDirs              string
	addrs                  []*buildkitAddr
	addrsRaw               string
	concurrency            int
	ctx                    context.Context
	failfast               bool
	oci                    bool
	bothFormats            bool
	oomCooldown            time.Duration
	resultPath             string
	logsPath               string
	vars                   map[string]string
	timeout                int
	verbose                bool
	target                 string
	skipFail               bool
	fromResultPath         string
	retry                  int
	dragonflySchedulerAddr string
	interval               int
	withFail               bool
}

// buildkitAddr represents a single resolved buildkitd endpoint.
type buildkitAddr struct {
	original      string
	addr          string
	nodeIP        string
	cooldown      time.Duration
	mu            sync.Mutex
	cooldownUntil time.Time
}

type addrSlot struct {
	addr *buildkitAddr
	sem  chan struct{}
}

type addrPoolSnapshot struct {
	slots []*addrSlot
	hash  *consistentHash
}

type addrPool struct {
	mu          sync.RWMutex
	concurrency int
	slots       []*addrSlot
	hash        *consistentHash
}

func (a *buildkitAddr) isInCooldown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Before(a.cooldownUntil)
}

func (a *buildkitAddr) setCooldown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cooldownUntil = time.Now().Add(a.cooldown)
}

func newAddrPool(addrs []*buildkitAddr, concurrency int) *addrPool {
	p := &addrPool{concurrency: concurrency}
	p.replace(addrs)
	return p
}

func (p *addrPool) snapshot() addrPoolSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	slots := append([]*addrSlot(nil), p.slots...)
	return addrPoolSnapshot{slots: slots, hash: p.hash}
}

func (p *addrPool) addresses() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slotAddressKeys(p.slots)
}

func (p *addrPool) replace(addrs []*buildkitAddr) {
	p.mu.Lock()
	defer p.mu.Unlock()

	existing := make(map[string]*addrSlot, len(p.slots))
	for _, slot := range p.slots {
		if slot != nil && slot.addr != nil {
			existing[slot.addr.addr] = slot
		}
	}

	newSlots := make([]*addrSlot, 0, len(addrs))
	for _, addr := range addrs {
		if addr == nil {
			continue
		}
		if slot, ok := existing[addr.addr]; ok {
			newSlots = append(newSlots, slot)
			continue
		}
		newSlots = append(newSlots, &addrSlot{
			addr: addr,
			sem:  make(chan struct{}, p.concurrency),
		})
	}

	sort.Slice(newSlots, func(i, j int) bool {
		return newSlots[i].addr.addr < newSlots[j].addr.addr
	})

	p.slots = newSlots
	if len(newSlots) == 0 {
		p.hash = nil
		return
	}
	p.hash = newConsistentHash(len(newSlots), 150)
}

// resultEntry is stored in LMDB and exported as JSONL.
type resultEntry struct {
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Elapsed    string `json:"elapsed"`
	Target     string `json:"target"`
	NodeIP     string `json:"node_ip,omitempty"`
	Success    bool   `json:"success"`
	Logs       string `json:"logs,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func (r *resultEntry) UnmarshalJSON(data []byte) error {
	type rawResultEntry struct {
		StartedAt  string          `json:"started_at"`
		FinishedAt string          `json:"finished_at"`
		Elapsed    json.RawMessage `json:"elapsed"`
		Target     string          `json:"target"`
		NodeIP     string          `json:"node_ip,omitempty"`
		Success    bool            `json:"success"`
		Logs       string          `json:"logs,omitempty"`
		Reason     string          `json:"reason,omitempty"`
	}

	var raw rawResultEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	elapsed, err := parseElapsedJSON(raw.Elapsed)
	if err != nil {
		return err
	}

	r.StartedAt = raw.StartedAt
	r.FinishedAt = raw.FinishedAt
	r.Elapsed = elapsed
	r.Target = raw.Target
	r.NodeIP = raw.NodeIP
	r.Success = raw.Success
	r.Logs = raw.Logs
	r.Reason = raw.Reason
	return nil
}

func parseElapsedJSON(data json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}

	var elapsedString string
	if err := json.Unmarshal(trimmed, &elapsedString); err == nil {
		return elapsedString, nil
	}

	var elapsedNumber json.Number
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&elapsedNumber); err != nil {
		return "", fmt.Errorf("decode elapsed: %w", err)
	}

	seconds, err := elapsedNumber.Float64()
	if err != nil {
		return "", fmt.Errorf("parse elapsed number: %w", err)
	}
	if seconds < 0 {
		return "", fmt.Errorf("parse elapsed number: negative value %v", seconds)
	}

	return formatElapsed(time.Duration(seconds * float64(time.Second))), nil
}

type imageMetadata struct {
	Target string `json:"target"`
}

type buildSpec struct {
	dir         string
	target      string
	scheduleKey string
	oci         bool
}

type buildJob struct {
	key         string
	scheduleKey string
	specs       []buildSpec
}

type buildOutputCapture struct {
	mu        sync.Mutex
	buffer    []byte
	start     int
	size      int
	truncated bool
}

type resultStore struct {
	db   *lmdbResultDB
	logs *failureLogStore
}

type buildResultReader interface {
	Get(target string) (resultEntry, bool, error)
}

type buildResultWriter interface {
	UpsertResult(entry resultEntry) error
}

type buildOutcomeState uint8

const (
	buildOutcomeUnknown buildOutcomeState = iota
	buildOutcomeFailed
	buildOutcomeSucceeded
)

type buildOutcomeCounters struct {
	mu        sync.Mutex
	total     int
	succeeded int
	failed    int
	states    map[string]buildOutcomeState
}

func newBuildOutcomeCounters(total int, states map[string]buildOutcomeState) *buildOutcomeCounters {
	counters := &buildOutcomeCounters{
		total:  total,
		states: make(map[string]buildOutcomeState, len(states)),
	}
	for target, state := range states {
		counters.states[target] = state
		switch state {
		case buildOutcomeSucceeded:
			counters.succeeded++
		case buildOutcomeFailed:
			counters.failed++
		}
	}
	return counters
}

func (c *buildOutcomeCounters) apply(target string, success bool) (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	previous := c.states[target]
	next := buildOutcomeFailed
	if success {
		next = buildOutcomeSucceeded
	}

	if previous != next {
		switch previous {
		case buildOutcomeSucceeded:
			c.succeeded--
		case buildOutcomeFailed:
			c.failed--
		}
		switch next {
		case buildOutcomeSucceeded:
			c.succeeded++
		case buildOutcomeFailed:
			c.failed++
		}
		c.states[target] = next
	}

	return c.succeeded, c.total, c.failed
}

func (c *buildOutcomeCounters) applyExisting(target string, state buildOutcomeState) (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	previous := c.states[target]
	if previous == state {
		return c.succeeded, c.total, c.failed
	}
	switch previous {
	case buildOutcomeSucceeded:
		c.succeeded--
	case buildOutcomeFailed:
		c.failed--
	}
	switch state {
	case buildOutcomeSucceeded:
		c.succeeded++
	case buildOutcomeFailed:
		c.failed++
	}
	c.states[target] = state
	return c.succeeded, c.total, c.failed
}

func (c *buildOutcomeCounters) decrementTotal() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total > 0 {
		c.total--
	}
	return c.succeeded, c.total, c.failed
}

func (c *buildOutcomeCounters) snapshot() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.succeeded, c.total, c.failed
}

// ----------------------------------------------------------------
// CLI
// ----------------------------------------------------------------

func main() {
	resetCommandStartTime(time.Now())
	app := newCLIApp()
	if err := app.Run(os.Args); err != nil {
		logError("%v", err)
		os.Exit(1)
	}
}

func newCLIApp() *cli.App {
	return &cli.App{
		Name:  "buildctl-batch",
		Usage: "batch helper for build, export, and preheat workflows",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "pprof-server",
				Usage: "listen address for net/http/pprof server, e.g. 0.0.0.0:5241",
			},
		},
		Before: func(c *cli.Context) error {
			if addr := resolvePprofServerAddr(c.String("pprof-server")); addr != "" {
				go func() {
					logInfo("Starting pprof server on %s", addr)
					if err := http.ListenAndServe(addr, nil); err != nil {
						logError("pprof server: %v", err)
					}
				}()
			}
			return nil
		},
		Commands: []*cli.Command{
			buildCLICommand(),
			exportCLICommand(),
			preheatCLICommand(),
			daemonCLICommand(),
		},
	}
}

func resolvePprofServerAddr(flagValue string) string {
	if trimmed := strings.TrimSpace(flagValue); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(os.Getenv(pprofServerEnv))
}

func buildCLICommand() *cli.Command {
	return &cli.Command{
		Name: "build",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "image-dirs", Usage: "path to a directory whose child directories each contain Dockerfile, metadata.json, and optional context"},
			&cli.StringFlag{Name: "addrs", Usage: "comma-separated buildkitd addresses; hostnames are resolved to IPs for scheduling"},
			&cli.StringSliceFlag{Name: "var", Usage: "replace occurrences of $KEY or ${KEY} in Dockerfile and metadata.json before build; repeatable, format KEY=value"},
			&cli.IntFlag{Name: "concurrency", Value: 1, Usage: "total number of concurrent buildctl invocations across all buildkitd addresses"},
			&cli.BoolFlag{Name: "fail-fast", Usage: "stop immediately after the first buildctl failure"},
			&cli.BoolFlag{Name: "oci", Usage: "build standard OCI images with gzip compression instead of nydus format"},
			&cli.BoolFlag{Name: "both-formats", Usage: "build both nydus and OCI images; runs buildctl twice per target"},
			&cli.DurationFlag{Name: "oom-cooldown", Value: defaultBuildkitOOMCooldown, Usage: "pause scheduling new tasks for the affected buildkit address after detecting an OOM-style connection refusal"},
			&cli.StringFlag{Name: "result", Value: "result.lmdb", Usage: "path to LMDB result database"},
			&cli.StringFlag{Name: "logs", Value: "logs.jsonl", Usage: "path to JSONL file storing failed build logs"},
			&cli.IntFlag{Name: "timeout", Value: 300, Usage: "kill a buildctl task if it runs longer than the given number of seconds; 0 disables timeout"},
			&cli.IntFlag{Name: "retry", Value: 0, Usage: "number of times to retry a failed build before giving up; 0 disables retry"},
			&cli.BoolFlag{Name: "skip-fail", Usage: "skip targets that previously failed (recorded in LMDB)"},
			&cli.BoolFlag{Name: "verbose", Usage: "stream buildctl subprocess output to the console"},
		},
		Action: func(c *cli.Context) error {
			addrs, err := parseBuildkitAddrs(c.String("addrs"))
			if err != nil {
				return err
			}
			buildVars, err := parseBuildVariables(c.StringSlice("var"))
			if err != nil {
				return err
			}
			oomCooldown := c.Duration("oom-cooldown")
			for _, addr := range addrs {
				addr.cooldown = oomCooldown
			}
			return runBuildCommand(options{
				imageDirs:   c.String("image-dirs"),
				addrs:       addrs,
				addrsRaw:    c.String("addrs"),
				concurrency: c.Int("concurrency"),
				failfast:    c.Bool("fail-fast"),
				oci:         c.Bool("oci"),
				bothFormats: c.Bool("both-formats"),
				oomCooldown: oomCooldown,
				resultPath:  c.String("result"),
				logsPath:    c.String("logs"),
				vars:        buildVars,
				timeout:     c.Int("timeout"),
				retry:       c.Int("retry"),
				verbose:     c.Bool("verbose"),
				target:      strings.TrimSpace(c.Args().First()),
				skipFail:    c.Bool("skip-fail"),
			})
		},
	}
}

func exportCLICommand() *cli.Command {
	return &cli.Command{
		Name: "export",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from-result", Value: "result.lmdb", Usage: "path to the LMDB result database to read"},
			&cli.StringFlag{Name: "result", Value: "result.jsonl", Usage: "path to write exported JSONL"},
			&cli.BoolFlag{Name: "oci", Usage: "export only OCI (non-nydus) targets without suffix filtering changes"},
			&cli.BoolFlag{Name: "with-fail", Usage: "also export failed targets"},
		},
		Action: func(c *cli.Context) error {
			return runExport(options{
				fromResultPath: c.String("from-result"),
				resultPath:     c.String("result"),
				oci:            c.Bool("oci"),
				withFail:       c.Bool("with-fail"),
			})
		},
	}
}

func preheatCLICommand() *cli.Command {
	return &cli.Command{
		Name: "preheat",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from-result", Value: "result.lmdb", Usage: "path to the LMDB result database to read and update"},
			&cli.StringFlag{Name: "dragonfly-scheduler-addr", Usage: "Dragonfly Scheduler gRPC address (e.g. 10.0.0.1:8002)"},
			&cli.IntFlag{Name: "concurrency", Value: 1, Usage: "number of concurrent grpcurl invocations"},
			&cli.IntFlag{Name: "interval", Value: 5, Usage: "minimum interval in seconds between starting grpcurl preheat tasks; 0 disables throttling"},
			&cli.IntFlag{Name: "timeout", Value: 5, Usage: "kill a grpcurl task if it runs longer than the given number of seconds; 0 disables timeout"},
			&cli.BoolFlag{Name: "fail-fast", Usage: "stop immediately after the first preheat failure"},
			&cli.BoolFlag{Name: "oci", Usage: "only preheat OCI (non-nydus) targets"},
			&cli.BoolFlag{Name: "verbose", Usage: "stream grpcurl subprocess output to the console"},
		},
		Action: func(c *cli.Context) error {
			return runPreheat(options{
				fromResultPath:         c.String("from-result"),
				dragonflySchedulerAddr: c.String("dragonfly-scheduler-addr"),
				concurrency:            c.Int("concurrency"),
				interval:               c.Int("interval"),
				timeout:                c.Int("timeout"),
				failfast:               c.Bool("fail-fast"),
				oci:                    c.Bool("oci"),
				verbose:                c.Bool("verbose"),
			})
		},
	}
}

// ----------------------------------------------------------------
// build
// ----------------------------------------------------------------

func runBuildCommand(opts options) error {
	if opts.ctx == nil {
		resetCommandStartTime(time.Now())
	}

	if len(opts.addrs) == 0 {
		return fmt.Errorf("--addrs is required")
	}

	buildModes, err := resolveBuildModes(opts.oci, opts.bothFormats)
	if err != nil {
		return err
	}
	if opts.imageDirs != "" {
		return runBuildCommandStreamingImageDirs(opts, buildModes)
	}

	specs, cleanup, err := loadBuildSpecs(opts.imageDirs, opts.target, buildModes, opts.vars)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	if len(specs) == 0 {
		logInfo("No targets to build")
		return nil
	}

	jobs := groupBuildSpecs(specs)
	if len(jobs) == 0 {
		logInfo("No targets to build")
		return nil
	}

	store, err := newResultStore(opts.resultPath, opts.logsPath)
	if err != nil {
		return err
	}
	defer store.Close()

	totalTargets := len(jobs)
	jobs, existingOutcomes, skippedSucceeded, skippedFailed, err := filterBuildJobs(jobs, store.db, opts.skipFail)
	if err != nil {
		return err
	}

	if skippedSucceeded > 0 || skippedFailed > 0 {
		logInfo("Skipped %d previously successful target(s) and %d previously failed target(s)", skippedSucceeded, skippedFailed)
	}
	if len(jobs) == 0 {
		logInfo("No targets to build")
		return nil
	}

	resetLogProgress(len(jobs))
	defer clearLogProgress()

	logInfo("Building %d target(s) across %d address(es), concurrency=%d, retry=%d",
		len(jobs), len(opts.addrs), opts.concurrency, opts.retry)

	addrPool := newAddrPool(opts.addrs, opts.concurrency)
	globalSem := make(chan struct{}, opts.concurrency)
	outcomeCounters := newBuildOutcomeCounters(totalTargets, existingOutcomes)

	baseCtx := opts.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	startBuildkitAddrRefresher(ctx, addrPool, opts.addrsRaw, opts.oomCooldown)

	if opts.ctx == nil {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			logInfo("Received interrupt, cancelling builds...")
			cancel()
		}()
	}

	type buildTask struct {
		job     buildJob
		attempt int
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		results  []resultEntry
		failed   atomic.Bool
		fatalMu  sync.Mutex
		fatalErr error
	)

	recordFatalErr := func(err error) {
		if err == nil {
			return
		}
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
	}

	// retryQueue collects failed tasks that still have retry budget.
	retryQueue := make(chan buildTask, len(jobs))

	// scheduleTask picks a slot via consistent hash and launches the build goroutine.
	scheduleTask := func(task buildTask) bool {
		if failed.Load() && opts.failfast {
			return false
		}
		if ctx.Err() != nil {
			return false
		}

		var slot *addrSlot
		for {
			if ctx.Err() != nil {
				return false
			}

			snapshot := addrPool.snapshot()
			slot = pickAvailableAddrSlot(snapshot, task.job.scheduleKey)
			if slot == nil {
				select {
				case <-time.After(200 * time.Millisecond):
				case <-ctx.Done():
					return false
				}
				continue
			}

			break
		}

		select {
		case globalSem <- struct{}{}:
		case <-ctx.Done():
			<-slot.sem
			return false
		}

		wg.Add(1)
		go func(task buildTask, slot *addrSlot) {
			defer wg.Done()
			defer func() { <-globalSem }()
			defer func() { <-slot.sem }()

			taskStartedAt := time.Now()
			taskEntry := resultEntry{Target: task.job.key}
			taskSuccess := true

			for idx, spec := range task.job.specs {
				entry := executeBuild(ctx, spec, slot.addr, opts)
				taskEntry.NodeIP = entry.NodeIP

				if !entry.Success && task.attempt < opts.retry {
					if entry.Logs != "" {
						if err := store.logs.AppendFailure(entry.Target, entry.Logs); err != nil {
							logError("Failed to append failure log for %s: %v", entry.Target, err)
						}
					}
					remainingSpecs := append([]buildSpec(nil), task.job.specs[idx:]...)
					logInfo("[RETRY %d/%d] %s", task.attempt+1, opts.retry, entry.Target)
					retryQueue <- buildTask{
						job: buildJob{
							key:         task.job.key,
							scheduleKey: task.job.scheduleKey,
							specs:       remainingSpecs,
						},
						attempt: task.attempt + 1,
					}
					return
				}

				if err := store.UpsertResult(entry); err != nil {
					persistErr := fmt.Errorf("store result for %s: %w", entry.Target, err)
					logError("Failed to store result for %s: %v", entry.Target, err)
					recordFatalErr(persistErr)
					failed.Store(true)
					cancel()
					return
				}

				if !entry.Success {
					taskSuccess = false
					break
				}
			}

			advanceLogProgress()

			taskEntry.Success = taskSuccess
			taskEntry.Elapsed = formatElapsed(time.Since(taskStartedAt))
			succeededCount, totalCount, failedCount := outcomeCounters.apply(task.job.key, taskSuccess)

			mu.Lock()
			results = append(results, taskEntry)
			mu.Unlock()

			printBuildResult(taskEntry, succeededCount, totalCount, failedCount)

			if !taskSuccess {
				failed.Store(true)
				if opts.failfast {
					cancel()
				}
			}
		}(task, slot)

		return true
	}

	// First pass: schedule all initial tasks.
	for _, job := range jobs {
		if !scheduleTask(buildTask{job: job, attempt: 0}) {
			break
		}
	}

	// Drain retry queue: wait for in-flight builds to finish, then re-schedule retries.
	for {
		wg.Wait()
		select {
		case task := <-retryQueue:
			// There may be more queued retries; drain them all before waiting again.
			tasks := []buildTask{task}
			for {
				select {
				case t := <-retryQueue:
					tasks = append(tasks, t)
				default:
					goto schedule
				}
			}
		schedule:
			for _, t := range tasks {
				if !scheduleTask(t) {
					break
				}
			}
		default:
			// No retries pending, we're done.
			goto done
		}
	}
done:

	printSummary(results)

	fatalMu.Lock()
	defer fatalMu.Unlock()
	if fatalErr != nil {
		return fatalErr
	}

	if failed.Load() {
		return fmt.Errorf("one or more builds failed")
	}
	return nil
}

func runBuildCommandStreamingImageDirs(opts options, buildModes []bool) error {
	store, err := newResultStore(opts.resultPath, opts.logsPath)
	if err != nil {
		return err
	}
	defer store.Close()

	entries, err := os.ReadDir(opts.imageDirs)
	if err != nil {
		return fmt.Errorf("read image-dirs: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("image-dirs %s does not contain any image directories", opts.imageDirs)
	}

	preparedRoot, err := os.MkdirTemp("", "buildctl-batch-image-dirs-")
	if err != nil {
		return fmt.Errorf("create temp image-dirs: %w", err)
	}
	defer os.RemoveAll(preparedRoot)

	targetFilters := make(map[string]struct{}, len(buildModes))
	if strings.TrimSpace(opts.target) != "" {
		for _, oci := range buildModes {
			targetFilters[normalizeTargetForMode(opts.target, oci)] = struct{}{}
		}
	}

	baseCtx := opts.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	if opts.ctx == nil {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			logInfo("Received interrupt, cancelling builds...")
			cancel()
		}()
	}

	resetLogProgress(len(entries))
	defer clearLogProgress()

	logInfo("Preparing %d source context(s) from %s with streaming scheduling", len(entries), opts.imageDirs)
	logInfo("Building up to %d target(s) across %d address(es), concurrency=%d, retry=%d",
		len(entries), len(opts.addrs), opts.concurrency, opts.retry)

	addrPool := newAddrPool(opts.addrs, opts.concurrency)
	startBuildkitAddrRefresher(ctx, addrPool, opts.addrsRaw, opts.oomCooldown)
	globalSem := make(chan struct{}, opts.concurrency)
	outcomeCounters := newBuildOutcomeCounters(len(entries), nil)

	type buildTask struct {
		job     buildJob
		attempt int
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		results  []resultEntry
		failed   atomic.Bool
		fatalMu  sync.Mutex
		fatalErr error
	)

	recordFatalErr := func(err error) {
		if err == nil {
			return
		}
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
	}

	retryQueue := make(chan buildTask, len(entries)+1)
	jobCh := make(chan buildJob, max(1, opts.concurrency))
	prepareErrCh := make(chan error, 1)

	go func() {
		defer close(jobCh)
		prepareErrCh <- streamPreparedBuildJobs(ctx, opts, buildModes, targetFilters, entries, preparedRoot, store, outcomeCounters, jobCh)
	}()

	scheduleTask := func(task buildTask) bool {
		if failed.Load() && opts.failfast {
			return false
		}
		if ctx.Err() != nil {
			return false
		}

		var slot *addrSlot
		for {
			if ctx.Err() != nil {
				return false
			}

			snapshot := addrPool.snapshot()
			slot = pickAvailableAddrSlot(snapshot, task.job.scheduleKey)
			if slot == nil {
				select {
				case <-time.After(200 * time.Millisecond):
				case <-ctx.Done():
					return false
				}
				continue
			}

			break
		}

		select {
		case globalSem <- struct{}{}:
		case <-ctx.Done():
			<-slot.sem
			return false
		}

		wg.Add(1)
		go func(task buildTask, slot *addrSlot) {
			defer wg.Done()
			defer func() { <-globalSem }()
			defer func() { <-slot.sem }()

			taskStartedAt := time.Now()
			taskEntry := resultEntry{Target: task.job.key}
			taskSuccess := true

			for idx, spec := range task.job.specs {
				entry := executeBuild(ctx, spec, slot.addr, opts)
				taskEntry.NodeIP = entry.NodeIP

				if !entry.Success && task.attempt < opts.retry {
					if entry.Logs != "" {
						if err := store.logs.AppendFailure(entry.Target, entry.Logs); err != nil {
							logError("Failed to append failure log for %s: %v", entry.Target, err)
						}
					}
					remainingSpecs := append([]buildSpec(nil), task.job.specs[idx:]...)
					logInfo("[RETRY %d/%d] %s", task.attempt+1, opts.retry, entry.Target)
					retryQueue <- buildTask{
						job: buildJob{
							key:         task.job.key,
							scheduleKey: task.job.scheduleKey,
							specs:       remainingSpecs,
						},
						attempt: task.attempt + 1,
					}
					return
				}

				if err := store.UpsertResult(entry); err != nil {
					persistErr := fmt.Errorf("store result for %s: %w", entry.Target, err)
					logError("Failed to store result for %s: %v", entry.Target, err)
					recordFatalErr(persistErr)
					failed.Store(true)
					cancel()
					return
				}

				if !entry.Success {
					taskSuccess = false
					break
				}
			}

			advanceLogProgress()

			taskEntry.Success = taskSuccess
			taskEntry.Elapsed = formatElapsed(time.Since(taskStartedAt))
			succeededCount, totalCount, failedCount := outcomeCounters.apply(task.job.key, taskSuccess)

			mu.Lock()
			results = append(results, taskEntry)
			mu.Unlock()

			printBuildResult(taskEntry, succeededCount, totalCount, failedCount)

			if !taskSuccess {
				failed.Store(true)
				if opts.failfast {
					cancel()
				}
			}
		}(task, slot)

		return true
	}

	scheduling := true
	for job := range jobCh {
		if scheduling {
			if !scheduleTask(buildTask{job: job, attempt: 0}) {
				scheduling = false
			}
		}
	}
	if err := <-prepareErrCh; err != nil {
		recordFatalErr(err)
		failed.Store(true)
		cancel()
	}

	for {
		wg.Wait()
		select {
		case task := <-retryQueue:
			tasks := []buildTask{task}
			for {
				select {
				case t := <-retryQueue:
					tasks = append(tasks, t)
				default:
					goto schedule
				}
			}
		schedule:
			for _, t := range tasks {
				if !scheduleTask(t) {
					break
				}
			}
		default:
			goto done
		}
	}
done:

	printSummary(results)

	fatalMu.Lock()
	defer fatalMu.Unlock()
	if fatalErr != nil {
		return fatalErr
	}
	if failed.Load() {
		return fmt.Errorf("one or more builds failed")
	}
	return nil
}

func streamPreparedBuildJobs(ctx context.Context, opts options, buildModes []bool, targetFilters map[string]struct{}, entries []os.DirEntry, preparedRoot string, store *resultStore, outcomeCounters *buildOutcomeCounters, out chan<- buildJob) error {
	for idx, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !entry.IsDir() {
			return fmt.Errorf("image-dirs %s must contain only directories, found %s", opts.imageDirs, entry.Name())
		}

		srcDir := filepath.Join(opts.imageDirs, entry.Name())
		if err := validateSourceImageDir(srcDir); err != nil {
			return err
		}

		dstDir := filepath.Join(preparedRoot, entry.Name())
		startedAt := time.Now()
		logInfo("[PREPARE %d/%d] %s: copy source context", idx+1, len(entries), entry.Name())
		if err := copyDirectory(srcDir, dstDir); err != nil {
			return fmt.Errorf("copy %s: %w", srcDir, err)
		}

		logInfo("[HASH %d/%d] %s: hashing source context", idx+1, len(entries), entry.Name())
		scheduleKey, err := sourceContextContentHash(dstDir)
		if err != nil {
			return err
		}
		logInfo("[HASH %d/%d] %s: done in %s key=%s", idx+1, len(entries), entry.Name(), formatElapsed(time.Since(startedAt)), shortScheduleKey(scheduleKey))

		if err := replaceBuildVariablesInFile(filepath.Join(dstDir, "Dockerfile"), opts.vars); err != nil {
			return err
		}
		if _, err := dockerfilepreprocess.PreprocessDockerfile(filepath.Join(dstDir, "Dockerfile")); err != nil {
			return err
		}
		if err := replaceBuildVariablesInFile(filepath.Join(dstDir, "metadata.json"), opts.vars); err != nil {
			return err
		}

		meta, err := loadImageMetadata(filepath.Join(dstDir, "metadata.json"))
		if err != nil {
			return err
		}
		if meta.Target == "" {
			return fmt.Errorf("%s has empty target in metadata.json", dstDir)
		}

		var specs []buildSpec
		for _, oci := range buildModes {
			normalizedTarget := normalizeTargetForMode(meta.Target, oci)
			if len(targetFilters) > 0 {
				if _, ok := targetFilters[normalizedTarget]; !ok {
					continue
				}
			}
			specs = append(specs, buildSpec{
				dir:         dstDir,
				target:      normalizedTarget,
				scheduleKey: scheduleKey,
				oci:         oci,
			})
		}
		if len(specs) == 0 {
			logInfo("[SKIP %d/%d] %s: target filter excluded all build modes", idx+1, len(entries), entry.Name())
			outcomeCounters.decrementTotal()
			advanceLogProgress()
			continue
		}

		jobs, existingOutcomes, skippedSucceeded, skippedFailed, err := filterBuildJobs(groupBuildSpecs(specs), store.db, opts.skipFail)
		if err != nil {
			return err
		}
		for target, state := range existingOutcomes {
			outcomeCounters.applyExisting(target, state)
		}
		if skippedSucceeded > 0 || skippedFailed > 0 {
			logInfo("[SKIP %d/%d] %s: skipped %d successful and %d failed target(s)", idx+1, len(entries), entry.Name(), skippedSucceeded, skippedFailed)
			advanceLogProgress()
		}
		for _, job := range jobs {
			logInfo("[SCHEDULE %d/%d] %s: target=%s key=%s", idx+1, len(entries), entry.Name(), job.key, shortScheduleKey(job.scheduleKey))
			select {
			case out <- job:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

func shortScheduleKey(key string) string {
	if len(key) <= len("source:")+12 || !strings.HasPrefix(key, "source:") {
		return key
	}
	return key[:len("source:")+12]
}

func loadBuildSpecs(imageDirs, target string, buildModes []bool, buildVars map[string]string) ([]buildSpec, func(), error) {
	if imageDirs == "" && target == "" {
		return nil, nil, fmt.Errorf("either --image-dirs or a positional [target] is required")
	}
	if len(buildModes) == 0 {
		return nil, nil, fmt.Errorf("at least one build mode is required")
	}

	if imageDirs == "" {
		specs := make([]buildSpec, 0, len(buildModes))
		for _, oci := range buildModes {
			normalizedTarget := normalizeTargetForMode(target, oci)
			specs = append(specs, buildSpec{
				target:      normalizedTarget,
				scheduleKey: stripNydusV3Suffix(normalizedTarget),
				oci:         oci,
			})
		}
		sort.Slice(specs, func(i, j int) bool {
			return specs[i].target < specs[j].target
		})
		return specs, nil, nil
	}

	targetFilters := make(map[string]struct{}, len(buildModes))
	if strings.TrimSpace(target) != "" {
		for _, oci := range buildModes {
			targetFilters[normalizeTargetForMode(target, oci)] = struct{}{}
		}
	}
	preparedRoot, scheduleKeys, err := prepareBuildImageDirs(imageDirs, buildVars)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		_ = os.RemoveAll(preparedRoot)
	}

	entries, err := os.ReadDir(preparedRoot)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("read image-dirs: %w", err)
	}
	if len(entries) == 0 {
		cleanup()
		return nil, nil, fmt.Errorf("image-dirs %s does not contain any image directories", imageDirs)
	}

	var specs []buildSpec
	for _, e := range entries {
		if !e.IsDir() {
			cleanup()
			return nil, nil, fmt.Errorf("image-dirs %s must contain only directories, found %s", imageDirs, e.Name())
		}
		dir := filepath.Join(preparedRoot, e.Name())
		metaPath := filepath.Join(dir, "metadata.json")

		meta, err := loadImageMetadata(metaPath)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if meta.Target == "" {
			cleanup()
			return nil, nil, fmt.Errorf("%s has empty target in metadata.json", dir)
		}
		scheduleKey := scheduleKeys[e.Name()]
		if scheduleKey == "" {
			cleanup()
			return nil, nil, fmt.Errorf("missing source context scheduling hash for %s", dir)
		}

		for _, oci := range buildModes {
			normalizedTarget := normalizeTargetForMode(meta.Target, oci)

			if len(targetFilters) > 0 {
				if _, ok := targetFilters[normalizedTarget]; !ok {
					continue
				}
			}

			specs = append(specs, buildSpec{
				dir:         dir,
				target:      normalizedTarget,
				scheduleKey: scheduleKey,
				oci:         oci,
			})
		}
	}

	sort.Slice(specs, func(i, j int) bool {
		return specs[i].target < specs[j].target
	})
	return specs, cleanup, nil
}

func groupBuildSpecs(specs []buildSpec) []buildJob {
	if len(specs) == 0 {
		return nil
	}

	grouped := make(map[string][]buildSpec, len(specs))
	for _, spec := range specs {
		key := stripNydusV3Suffix(spec.target)
		grouped[key] = append(grouped[key], spec)
	}

	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	jobs := make([]buildJob, 0, len(keys))
	for _, key := range keys {
		jobSpecs := append([]buildSpec(nil), grouped[key]...)
		sort.SliceStable(jobSpecs, func(i, j int) bool {
			if jobSpecs[i].oci != jobSpecs[j].oci {
				return !jobSpecs[i].oci
			}
			return jobSpecs[i].target < jobSpecs[j].target
		})
		scheduleKey := key
		if len(jobSpecs) > 0 && strings.TrimSpace(jobSpecs[0].scheduleKey) != "" {
			scheduleKey = jobSpecs[0].scheduleKey
		}
		jobs = append(jobs, buildJob{key: key, scheduleKey: scheduleKey, specs: jobSpecs})
	}

	return jobs
}

func filterBuildJobs(jobs []buildJob, results buildResultReader, skipFail bool) ([]buildJob, map[string]buildOutcomeState, int, int, error) {
	existingOutcomes := make(map[string]buildOutcomeState, len(jobs))
	filtered := make([]buildJob, 0, len(jobs))
	var skippedSucceeded, skippedFailed int

	for _, job := range jobs {
		pendingSpecs := make([]buildSpec, 0, len(job.specs))
		allSucceeded := len(job.specs) > 0
		anyFailed := false

		for _, spec := range job.specs {
			entry, found, err := results.Get(spec.target)
			if err != nil {
				return nil, nil, 0, 0, err
			}
			if !found {
				allSucceeded = false
				pendingSpecs = append(pendingSpecs, spec)
				continue
			}
			if entry.Success {
				continue
			}

			allSucceeded = false
			anyFailed = true
			if !skipFail {
				pendingSpecs = append(pendingSpecs, spec)
			}
		}

		if allSucceeded {
			existingOutcomes[job.key] = buildOutcomeSucceeded
			skippedSucceeded++
			continue
		}
		if anyFailed && skipFail {
			existingOutcomes[job.key] = buildOutcomeFailed
			skippedFailed++
			continue
		}

		filtered = append(filtered, buildJob{key: job.key, scheduleKey: job.scheduleKey, specs: pendingSpecs})
	}

	return filtered, existingOutcomes, skippedSucceeded, skippedFailed, nil
}

func prepareBuildImageDirs(imageDirs string, buildVars map[string]string) (string, map[string]string, error) {
	entries, err := os.ReadDir(imageDirs)
	if err != nil {
		return "", nil, fmt.Errorf("read image-dirs: %w", err)
	}
	if len(entries) == 0 {
		return "", nil, fmt.Errorf("image-dirs %s does not contain any image directories", imageDirs)
	}

	preparedRoot, err := os.MkdirTemp("", "buildctl-batch-image-dirs-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp image-dirs: %w", err)
	}
	scheduleKeys := make(map[string]string, len(entries))

	cleanup := func() {
		_ = os.RemoveAll(preparedRoot)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			cleanup()
			return "", nil, fmt.Errorf("image-dirs %s must contain only directories, found %s", imageDirs, entry.Name())
		}

		srcDir := filepath.Join(imageDirs, entry.Name())
		if err := validateSourceImageDir(srcDir); err != nil {
			cleanup()
			return "", nil, err
		}

		dstDir := filepath.Join(preparedRoot, entry.Name())
		if err := copyDirectory(srcDir, dstDir); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("copy %s: %w", srcDir, err)
		}

		scheduleKey, err := sourceContextContentHash(dstDir)
		if err != nil {
			cleanup()
			return "", nil, err
		}
		scheduleKeys[entry.Name()] = scheduleKey

		if err := replaceBuildVariablesInFile(filepath.Join(dstDir, "Dockerfile"), buildVars); err != nil {
			cleanup()
			return "", nil, err
		}
		if _, err := dockerfilepreprocess.PreprocessDockerfile(filepath.Join(dstDir, "Dockerfile")); err != nil {
			cleanup()
			return "", nil, err
		}
		if err := replaceBuildVariablesInFile(filepath.Join(dstDir, "metadata.json"), buildVars); err != nil {
			cleanup()
			return "", nil, err
		}
	}

	return preparedRoot, scheduleKeys, nil
}

func sourceContextContentHash(dir string) (string, error) {
	var paths []string
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if !entry.IsDir() && !mode.IsRegular() && mode&os.ModeSymlink == 0 {
			return nil
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk source context for scheduling hash: %w", err)
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, path := range paths {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return "", fmt.Errorf("rel source context path: %w", err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("stat source context path %s: %w", rel, err)
		}
		rel = filepath.ToSlash(rel)
		mode := info.Mode()
		if mode.IsDir() {
			fmt.Fprintf(h, "dir:%s\x00%o\x00", rel, mode.Perm())
			continue
		}
		if mode&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return "", fmt.Errorf("read source context symlink %s: %w", rel, err)
			}
			fmt.Fprintf(h, "symlink:%s\x00%o\x00%s\x00", rel, mode.Perm(), target)
			continue
		}
		fmt.Fprintf(h, "file:%s\x00%o\x00", rel, mode.Perm())
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open source context file %s: %w", rel, err)
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("hash source context file %s: %w", rel, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close source context file %s: %w", rel, closeErr)
		}
		_, _ = h.Write([]byte{0})
	}

	return fmt.Sprintf("source:%x", h.Sum(nil)), nil
}

func validateSourceImageDir(dir string) error {
	if err := requireRegularFile(filepath.Join(dir, "Dockerfile")); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if err := requireRegularFile(filepath.Join(dir, "metadata.json")); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	return nil
}

func requireRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing required file %s", filepath.Base(path))
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("required file %s must be a regular file", filepath.Base(path))
	}
	return nil
}

func replaceBuildVariablesInFile(path string, buildVars map[string]string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	replaced, err := replaceBuildVariables(raw, path, buildVars)
	if err != nil {
		return err
	}
	if bytes.Equal(raw, replaced) {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, replaced, info.Mode())
}

func replaceBuildVariables(raw []byte, path string, buildVars map[string]string) ([]byte, error) {
	if len(buildVars) == 0 {
		return raw, nil
	}

	content := string(raw)
	for key, value := range buildVars {
		dollarToken := "$" + key
		braceToken := "${" + key + "}"
		content = strings.ReplaceAll(content, braceToken, value)
		content = strings.ReplaceAll(content, dollarToken, value)
	}

	unresolved := findUnresolvedBuildVariables(content)
	if len(unresolved) > 0 {
		missing := make([]string, 0, len(unresolved))
		for _, token := range unresolved {
			if _, ok := buildVars[token]; !ok {
				missing = append(missing, token)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, fmt.Errorf("%s contains unresolved build variable(s): %s", path, strings.Join(uniqueStrings(missing), ", "))
		}
	}

	return []byte(content), nil
}

func parseBuildVariables(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}

	buildVars := make(map[string]string, len(items))
	for _, item := range items {
		key, value, found := strings.Cut(item, "=")
		if !found {
			return nil, fmt.Errorf("invalid --var %q, expected KEY=value", item)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("invalid --var %q, key must not be empty", item)
		}
		if !strings.HasPrefix(key, "BUILDCTL_BATCH_") {
			return nil, fmt.Errorf("invalid --var %q, key must start with BUILDCTL_BATCH_", item)
		}
		buildVars[key] = value
	}
	return buildVars, nil
}

func findUnresolvedBuildVariables(content string) []string {
	matches := buildctlBatchVarPattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 {
			result = append(result, match[1])
		}
	}
	return result
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	last := ""
	for _, value := range values {
		if value == last {
			continue
		}
		result = append(result, value)
		last = value
	}
	return result
}

func loadImageMetadata(metaPath string) (imageMetadata, error) {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("read %s: %w", metaPath, err)
	}
	var meta imageMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return imageMetadata{}, fmt.Errorf("invalid %s: %w", metaPath, err)
	}
	meta.Target = strings.TrimSpace(meta.Target)
	if meta.Target == "" {
		return imageMetadata{}, fmt.Errorf("%s has empty target", metaPath)
	}
	return meta, nil
}

func copyDirectory(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(dst, relPath)

		info, err := d.Info()
		if err != nil {
			return err
		}

		if d.IsDir() {
			return os.MkdirAll(targetPath, info.Mode())
		}
		if d.Type()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, targetPath)
		}
		return copyFile(path, targetPath, info.Mode())
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func hasNydusV3Suffix(target string) bool {
	return strings.HasSuffix(strings.TrimSpace(target), nydusV3TargetSuffix)
}

func ensureNydusV3Suffix(target string) string {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" || hasNydusV3Suffix(trimmed) {
		return trimmed
	}
	return trimmed + nydusV3TargetSuffix
}

func stripNydusV3Suffix(target string) string {
	return strings.TrimSuffix(strings.TrimSpace(target), nydusV3TargetSuffix)
}

func normalizeTargetForMode(target string, oci bool) string {
	if oci {
		return stripNydusV3Suffix(target)
	}
	return ensureNydusV3Suffix(target)
}

func resolveBuildModes(oci, bothFormats bool) ([]bool, error) {
	if oci && bothFormats {
		return nil, fmt.Errorf("--oci and --both-formats are mutually exclusive")
	}
	if bothFormats {
		return []bool{false, true}, nil
	}
	if oci {
		return []bool{true}, nil
	}
	return []bool{false}, nil
}

func targetMatchesMode(target string, oci bool) bool {
	if oci {
		return !hasNydusV3Suffix(target)
	}
	return hasNydusV3Suffix(target)
}

func newBuildOutputCapture() (*buildOutputCapture, error) {
	return &buildOutputCapture{buffer: make([]byte, maxBuildLogBytes)}, nil
}

func (c *buildOutputCapture) writer(console io.Writer) io.Writer {
	if console != nil {
		return io.MultiWriter(console, c)
	}
	return c
}

func (c *buildOutputCapture) closeAndReadTail(readLogs bool) (string, error) {
	if c == nil || !readLogs {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, c.size)
	if c.size > 0 {
		first := copy(out, c.buffer[c.start:])
		copy(out[first:], c.buffer[:c.size-first])
	}
	if c.truncated {
		return fmt.Sprintf("[buildctl output truncated to last %d bytes]\n%s", maxBuildLogBytes, out), nil
	}
	return string(out), nil
}

func (c *buildOutputCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	written := len(data)
	if len(data) >= len(c.buffer) {
		copy(c.buffer, data[len(data)-len(c.buffer):])
		c.start = 0
		c.size = len(c.buffer)
		c.truncated = true
		return written, nil
	}
	if c.size+len(data) > len(c.buffer) {
		drop := c.size + len(data) - len(c.buffer)
		c.start = (c.start + drop) % len(c.buffer)
		c.size -= drop
		c.truncated = true
	}
	end := (c.start + c.size) % len(c.buffer)
	first := copy(c.buffer[end:], data)
	copy(c.buffer, data[first:])
	c.size += len(data)
	return written, nil
}

func readFileTail(path string, maxBytes int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	start := int64(0)
	truncated := false
	if maxBytes > 0 && size > maxBytes {
		start = size - maxBytes
		truncated = true
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}

	var out bytes.Buffer
	if truncated {
		fmt.Fprintf(&out, "[buildctl output truncated to last %d bytes]\n", maxBytes)
	}
	if _, err := io.Copy(&out, file); err != nil {
		return "", err
	}
	return out.String(), nil
}

func executeBuild(ctx context.Context, spec buildSpec, addr *buildkitAddr, opts options) resultEntry {
	startedAt := time.Now()
	nodeIP := addr.nodeIP
	if strings.TrimSpace(nodeIP) == "" {
		nodeIP = nodeIPFromBuildkitAddr(addr.addr)
	}
	if nodeIP == "" {
		nodeIP = "unknown"
	}

	var buildCtx context.Context
	var buildCancel context.CancelFunc
	if opts.timeout > 0 {
		buildCtx, buildCancel = context.WithTimeout(ctx, time.Duration(opts.timeout)*time.Second)
	} else {
		buildCtx, buildCancel = context.WithCancel(ctx)
	}
	defer buildCancel()

	args := buildCommandArgs(spec, addr)

	cmd := exec.CommandContext(buildCtx, "buildctl", args...)
	output, outputErr := newBuildOutputCapture()
	if outputErr != nil {
		return resultEntry{
			StartedAt:  startedAt.Format(time.RFC3339),
			FinishedAt: time.Now().Format(time.RFC3339),
			Elapsed:    formatElapsed(time.Since(startedAt)),
			Target:     spec.target,
			NodeIP:     nodeIP,
			Success:    false,
			Reason:     fmt.Sprintf("create build output capture: %v", outputErr),
		}
	}
	cmd.Stdout = output.writer(nil)
	if opts.verbose {
		cmd.Stdout = output.writer(os.Stdout)
	}
	cmd.Stderr = output.writer(nil)
	if opts.verbose {
		cmd.Stderr = output.writer(os.Stderr)
	}

	err := cmd.Run()
	finishedAt := time.Now()
	elapsed := finishedAt.Sub(startedAt)
	logs, logErr := output.closeAndReadTail(err != nil)

	entry := resultEntry{
		StartedAt:  startedAt.Format(time.RFC3339),
		FinishedAt: finishedAt.Format(time.RFC3339),
		Elapsed:    formatElapsed(elapsed),
		Target:     spec.target,
		NodeIP:     nodeIP,
		Success:    err == nil,
	}

	if err != nil {
		entry.Logs = logs
		entry.Reason = err.Error()
		if logErr != nil {
			entry.Reason = fmt.Sprintf("%s; read build output: %v", entry.Reason, logErr)
		}

		lowerLogs := strings.ToLower(logs)
		if strings.Contains(lowerLogs, "connection refused") {
			logInfo("Detected OOM-style failure for %s, cooling down %s for %s",
				spec.target, addr.addr, addr.cooldown)
			addr.setCooldown()
		}
	}

	return entry
}

func buildCommandArgs(spec buildSpec, addr *buildkitAddr) []string {
	args := []string{
		"--addr", addr.addr,
		"build",
		"--frontend=dockerfile.v0",
	}

	if spec.dir != "" {
		args = append(args,
			"--local", "context="+spec.dir,
			"--local", "dockerfile="+spec.dir,
		)
	}

	output := fmt.Sprintf("type=image,name=%s,push=true,force-compression=true,oci-mediatypes=true", spec.target)
	if spec.oci {
		output += ",compression=gzip"
	} else {
		output += ",compression=nydus,fs-version=5"
	}
	args = append(args, "--output", output)

	return args
}

// ----------------------------------------------------------------
// export
// ----------------------------------------------------------------

func runExport(opts options) error {
	resetCommandStartTime(time.Now())

	db, err := openLMDBResultDB(opts.fromResultPath)
	if err != nil {
		return fmt.Errorf("open result database: %w", err)
	}
	defer db.Close()

	entries, err := db.All()
	if err != nil {
		return err
	}

	exportableCount := 0
	for _, entry := range entries {
		if !targetMatchesMode(entry.Target, opts.oci) {
			continue
		}
		if !entry.Success && !opts.withFail {
			continue
		}
		exportableCount++
	}
	resetLogProgress(exportableCount)
	defer clearLogProgress()

	file, err := os.Create(opts.resultPath)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	var exported int
	for _, entry := range entries {
		if !targetMatchesMode(entry.Target, opts.oci) {
			continue
		}
		if !entry.Success && !opts.withFail {
			continue
		}
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		writer.Write(payload)
		writer.WriteByte('\n')
		exported++
		advanceLogProgress()
	}

	if err := writer.Flush(); err != nil {
		return err
	}

	logInfo("Exported %d entries to %s", exported, opts.resultPath)
	return nil
}

// ----------------------------------------------------------------
// preheat
// ----------------------------------------------------------------

func runPreheat(opts options) error {
	resetCommandStartTime(time.Now())

	if opts.dragonflySchedulerAddr == "" {
		return fmt.Errorf("--dragonfly-scheduler-addr is required")
	}

	db, err := openLMDBResultDB(opts.fromResultPath)
	if err != nil {
		return fmt.Errorf("open result database: %w", err)
	}
	defer db.Close()

	entries, err := db.All()
	if err != nil {
		return err
	}

	var targets []resultEntry
	for _, entry := range entries {
		if !entry.Success {
			continue
		}
		if !targetMatchesMode(entry.Target, opts.oci) {
			continue
		}
		targets = append(targets, entry)
	}

	if len(targets) == 0 {
		logInfo("No targets to preheat")
		return nil
	}

	resetLogProgress(len(targets))
	defer clearLogProgress()

	logInfo("Preheating %d target(s) with concurrency=%d", len(targets), opts.concurrency)

	sem := make(chan struct{}, opts.concurrency)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		anyError bool
		lastTime time.Time
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, entry := range targets {
		if anyError && opts.failfast {
			break
		}
		if ctx.Err() != nil {
			break
		}

		// Throttle based on --interval.
		if opts.interval > 0 {
			mu.Lock()
			since := time.Since(lastTime)
			wait := time.Duration(opts.interval)*time.Second - since
			if wait > 0 {
				mu.Unlock()
				select {
				case <-time.After(wait):
				case <-ctx.Done():
				}
			} else {
				mu.Unlock()
			}
			mu.Lock()
			lastTime = time.Now()
			mu.Unlock()
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			defer func() { <-sem }()

			err := executePreheat(ctx, target, opts)
			advanceLogProgress()
			if err != nil {
				logError("Preheat failed for %s: %v", target, err)
				mu.Lock()
				anyError = true
				mu.Unlock()
				if opts.failfast {
					cancel()
				}
			} else {
				logInfo("Preheated: %s", target)
			}
		}(entry.Target)
	}

	wg.Wait()
	if anyError {
		return fmt.Errorf("one or more preheat tasks failed")
	}
	return nil
}

func executePreheat(ctx context.Context, target string, opts options) error {
	var pCtx context.Context
	var pCancel context.CancelFunc
	if opts.timeout > 0 {
		pCtx, pCancel = context.WithTimeout(ctx, time.Duration(opts.timeout)*time.Second)
	} else {
		pCtx, pCancel = context.WithCancel(ctx)
	}
	defer pCancel()

	preheatURL, err := buildPreheatURL(target)
	if err != nil {
		return err
	}

	reqBody := map[string]any{
		"url":                preheatURL,
		"username":           "",
		"password":           "",
		"scope":              "all_seed_peers",
		"insecureSkipVerify": false,
	}
	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	args := []string{
		"-plaintext",
		"-d", "@",
		opts.dragonflySchedulerAddr,
		"scheduler.v2.Scheduler.PreheatImage",
	}

	cmd := exec.CommandContext(pCtx, "grpcurl", args...)
	cmd.Stdin = bytes.NewReader(reqJSON)
	var outputBuf bytes.Buffer
	if opts.verbose {
		cmd.Stdout = io.MultiWriter(os.Stdout, &outputBuf)
		cmd.Stderr = io.MultiWriter(os.Stderr, &outputBuf)
	} else {
		cmd.Stdout = &outputBuf
		cmd.Stderr = &outputBuf
	}

	return cmd.Run()
}

func buildPreheatURL(target string) (string, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return "", fmt.Errorf("preheat target is empty")
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return trimmed, nil
	}

	normalized := strings.TrimPrefix(strings.TrimPrefix(trimmed, "docker://"), "oci://")
	firstSlash := strings.IndexByte(normalized, '/')
	if firstSlash <= 0 || firstSlash == len(normalized)-1 {
		return "", fmt.Errorf("preheat target %q is not a fully qualified image reference", target)
	}

	registry := normalized[:firstSlash]
	remainder := normalized[firstSlash+1:]
	if !strings.Contains(registry, ".") && !strings.Contains(registry, ":") && registry != "localhost" {
		return "", fmt.Errorf("preheat target %q is not a fully qualified image reference", target)
	}

	repo := remainder
	reference := "latest"
	if at := strings.LastIndexByte(remainder, '@'); at >= 0 {
		repo = remainder[:at]
		reference = remainder[at+1:]
	} else {
		lastSlash := strings.LastIndexByte(remainder, '/')
		lastColon := strings.LastIndexByte(remainder, ':')
		if lastColon > lastSlash {
			repo = remainder[:lastColon]
			reference = remainder[lastColon+1:]
		}
	}

	if repo == "" || reference == "" {
		return "", fmt.Errorf("preheat target %q is not a valid image reference", target)
	}

	return fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repo, reference), nil
}

// ----------------------------------------------------------------
// consistent hashing
// ----------------------------------------------------------------

// consistentHash maps keys to node indices using a hash ring with virtual nodes,
// ensuring the same target is consistently scheduled to the same buildkitd address.
type consistentHash struct {
	ring     []uint32
	nodeMap  map[uint32]int
	numNodes int
}

func newConsistentHash(numNodes, replicas int) *consistentHash {
	ch := &consistentHash{
		nodeMap:  make(map[uint32]int),
		numNodes: numNodes,
	}
	for i := 0; i < numNodes; i++ {
		for r := 0; r < replicas; r++ {
			h := fnv1a(fmt.Sprintf("node-%d-replica-%d", i, r))
			ch.ring = append(ch.ring, h)
			ch.nodeMap[h] = i
		}
	}
	sort.Slice(ch.ring, func(a, b int) bool { return ch.ring[a] < ch.ring[b] })
	return ch
}

// getSlotOrder returns node indices in preference order for the given key.
// The first element is the primary; subsequent elements are fallbacks.
func (ch *consistentHash) getSlotOrder(key string) []int {
	if len(ch.ring) == 0 {
		return nil
	}
	h := fnv1a(key)
	idx := sort.Search(len(ch.ring), func(i int) bool { return ch.ring[i] >= h })
	if idx >= len(ch.ring) {
		idx = 0
	}

	seen := make(map[int]bool)
	order := make([]int, 0, ch.numNodes)
	for len(order) < ch.numNodes {
		nodeIdx := ch.nodeMap[ch.ring[idx%len(ch.ring)]]
		if !seen[nodeIdx] {
			seen[nodeIdx] = true
			order = append(order, nodeIdx)
		}
		idx++
	}
	return order
}

func fnv1a(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func pickAddrSlot(snapshot addrPoolSnapshot, key string) *addrSlot {
	if len(snapshot.slots) == 0 || snapshot.hash == nil {
		return nil
	}

	order := snapshot.hash.getSlotOrder(key)
	if len(order) == 0 {
		return nil
	}

	var fallback *addrSlot
	for _, idx := range order {
		if idx < 0 || idx >= len(snapshot.slots) {
			continue
		}
		slot := snapshot.slots[idx]
		if slot == nil || slot.addr == nil {
			continue
		}
		if fallback == nil {
			fallback = slot
		}
		if !slot.addr.isInCooldown() {
			return slot
		}
	}

	return fallback
}

func pickAvailableAddrSlot(snapshot addrPoolSnapshot, key string) *addrSlot {
	if len(snapshot.slots) == 0 || snapshot.hash == nil {
		return nil
	}

	order := snapshot.hash.getSlotOrder(key)
	if len(order) == 0 {
		return nil
	}

	for _, requireReady := range []bool{true, false} {
		for _, idx := range order {
			if idx < 0 || idx >= len(snapshot.slots) {
				continue
			}
			slot := snapshot.slots[idx]
			if slot == nil || slot.addr == nil {
				continue
			}
			if requireReady && slot.addr.isInCooldown() {
				continue
			}
			if !tryAcquireAddrSlot(slot.sem) {
				continue
			}
			return slot
		}
	}

	return nil
}

func tryAcquireAddrSlot(sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func startBuildkitAddrRefresher(ctx context.Context, pool *addrPool, addrsRaw string, oomCooldown time.Duration) {
	if strings.TrimSpace(addrsRaw) == "" {
		return
	}

	go func() {
		ticker := time.NewTicker(defaultBuildkitAddrRefreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			refreshedAddrs, err := parseBuildkitAddrs(addrsRaw)
			if err != nil {
				logError("Failed to refresh buildkit addresses from %q: %v", addrsRaw, err)
				continue
			}
			for _, addr := range refreshedAddrs {
				if addr != nil {
					addr.cooldown = oomCooldown
				}
			}

			before := pool.addresses()
			pool.replace(refreshedAddrs)
			after := pool.addresses()
			if !sameStringSlice(before, after) {
				logInfo("Refreshed buildkit address pool: %d -> %d endpoint(s): [%s] -> [%s]",
					len(before), len(after), strings.Join(before, ", "), strings.Join(after, ", "))
			}
		}
	}()
}

func slotAddressKeys(slots []*addrSlot) []string {
	keys := make([]string, 0, len(slots))
	for _, slot := range slots {
		if slot == nil || slot.addr == nil {
			continue
		}
		keys = append(keys, slot.addr.addr)
	}
	return keys
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------------
// address resolution
// ----------------------------------------------------------------

func parseBuildkitAddrs(s string) ([]*buildkitAddr, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("--addrs is required")
	}

	parts := strings.Split(s, ",")
	var addrs []*buildkitAddr

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		addr := part
		if !strings.Contains(addr, "://") {
			addr = "tcp://" + addr
		}

		hostPort := strings.TrimPrefix(addr, "tcp://")
		host, port, err := net.SplitHostPort(hostPort)
		if err != nil {
			addrs = append(addrs, &buildkitAddr{original: part, addr: addr, nodeIP: nodeIPFromBuildkitAddr(addr)})
			continue
		}

		if ip := net.ParseIP(host); ip != nil {
			addrs = append(addrs, &buildkitAddr{original: part, addr: addr, nodeIP: ip.String()})
			continue
		}

		ips, err := net.LookupHost(host)
		if err != nil {
			addrs = append(addrs, &buildkitAddr{original: part, addr: addr, nodeIP: nodeIPFromBuildkitAddr(addr)})
			continue
		}

		for _, ip := range ips {
			resolved := fmt.Sprintf("tcp://%s", net.JoinHostPort(ip, port))
			addrs = append(addrs, &buildkitAddr{original: part, addr: resolved, nodeIP: ip})
		}
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("no valid buildkitd addresses")
	}
	sort.Slice(addrs, func(i, j int) bool {
		return addrs[i].addr < addrs[j].addr
	})
	return addrs, nil
}

func nodeIPFromBuildkitAddr(addr string) string {
	hostPort := strings.TrimPrefix(strings.TrimSpace(addr), "tcp://")
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return ""
}

// ----------------------------------------------------------------
// result store
// ----------------------------------------------------------------

func newResultStore(resultPath, logsPath string) (*resultStore, error) {
	db, err := openLMDBResultDB(resultPath)
	if err != nil {
		return nil, err
	}
	return &resultStore{
		db:   db,
		logs: newFailureLogStore(logsPath),
	}, nil
}

func (rs *resultStore) UpsertResult(entry resultEntry) error {
	// Write failure log BEFORE writing to LMDB.
	if !entry.Success && entry.Logs != "" {
		if err := rs.logs.AppendFailure(entry.Target, entry.Logs); err != nil {
			logError("Failed to append failure log for %s: %v", entry.Target, err)
		}
		// Clear verbose logs before persisting in LMDB to save space.
		entry.Logs = ""
	}
	return rs.db.Put(entry)
}

func persistBuildResult(store buildResultWriter, counters *buildOutcomeCounters, entry resultEntry) (int, int, int, error) {
	if err := store.UpsertResult(entry); err != nil {
		return 0, 0, 0, fmt.Errorf("store result for %s: %w", entry.Target, err)
	}
	succeeded, total, failed := counters.apply(entry.Target, entry.Success)
	return succeeded, total, failed, nil
}

func (rs *resultStore) Close() error {
	if rs.db != nil {
		return rs.db.Close()
	}
	return nil
}

// ----------------------------------------------------------------
// helpers
// ----------------------------------------------------------------

func resolveOptionalPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	return filepath.Abs(path)
}

func printBuildResult(entry resultEntry, succeededCount, totalCount, failedCount int) {
	status := "OK"
	if !entry.Success {
		status = "FAIL"
	}
	nodeIP := entry.NodeIP
	if strings.TrimSpace(nodeIP) == "" {
		nodeIP = "unknown"
	}
	logInfo("[%s] success=%d/%d fail=%d target=%s node-ip=%s elapsed=%s", status, succeededCount, totalCount, failedCount, entry.Target, nodeIP, entry.Elapsed)
}

func printSummary(results []resultEntry) {
	var succeeded, failed int
	for _, r := range results {
		if r.Success {
			succeeded++
		} else {
			failed++
		}
	}
	logInfo("Summary: %d total, %d succeeded, %d failed", len(results), succeeded, failed)
}

func resetLogProgress(total int) {
	logProgressState.current.Store(0)
	logProgressState.total.Store(int64(total))
}

func advanceLogProgress() int64 {
	return logProgressState.current.Add(1)
}

func clearLogProgress() {
	logProgressState.current.Store(0)
	logProgressState.total.Store(0)
}

func resetCommandStartTime(start time.Time) {
	commandStartUnixNano.Store(start.UnixNano())
}

func currentCommandStartTime() time.Time {
	unixNano := commandStartUnixNano.Load()
	if unixNano == 0 {
		return time.Now()
	}
	return time.Unix(0, unixNano)
}

func formatLogElapsed(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(100 * time.Millisecond).String()
}

func formatElapsed(d time.Duration) string {
	s := roundElapsedSeconds(d)
	if s < 60 {
		return fmt.Sprintf("%.1fs", s)
	}
	m := int(s) / 60
	rs := s - float64(m*60)
	return fmt.Sprintf("%dm%.1fs", m, rs)
}

func roundElapsedSeconds(d time.Duration) float64 {
	return math.Round(d.Seconds()*10) / 10
}

func logInfo(format string, args ...any) {
	logWithLevel("INFO", format, args...)
}

func logError(format string, args ...any) {
	logWithLevel("ERROR", format, args...)
}

func logWithLevel(level string, format string, args ...any) {
	now := time.Now().Format(time.RFC3339)
	elapsed := formatLogElapsed(time.Since(currentCommandStartTime()))
	current := logProgressState.current.Load()
	total := logProgressState.total.Load()
	prefix := fmt.Sprintf("[%s] [%s] [%d/%d] [%s] ", now, elapsed, current, total, level)
	fmt.Fprintf(os.Stderr, prefix+format+"\n", args...)
}
