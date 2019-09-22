package command

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/api"
	gatedwriter "github.com/hashicorp/vault/helper/gated-writer"
	"github.com/hashicorp/vault/sdk/helper/logging"
	"github.com/hashicorp/vault/sdk/helper/strutil"
	"github.com/hashicorp/vault/sdk/version"
	"github.com/mholt/archiver"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

const (
	// debugIndexVersion is tracks the canonical version in the index file
	// for compatibility with future format/layout changes on the bundle.
	debugIndexVersion = 1

	// debugMinInterval is the minimum acceptable interval capture value. This
	// value applies to duration and all interval-related flags.
	debugMinInterval = 5 * time.Second

	// debugDurationGrace is the grace period added to duration to allow for
	// "last frame" capture if the interval falls into the last duration time
	// value. For instance, using default values, adding a grace duration lets
	// the command capture 5 intervals (0, 30, 60, 90, and 120th second) before
	// exiting.
	debugDurationGrace = 1 * time.Second

	// debugCompressionExt is the default compression extension used if
	// compression is enabled.
	debugCompressionExt = ".tar.gz"

	// fileFriendlyTimeFormat is the time format used for file and directory
	// naming.
	fileFriendlyTimeFormat = "2006-01-02T15-04-05Z"
)

// debugIndex represents the data in the index file
type debugIndex struct {
	VaultAddress    string                 `json:"vault_address"`
	Version         int                    `json:"version"`
	ClientVersion   string                 `json:"client_version"`
	Timestamp       time.Time              `json:"timestamp"`
	Duration        int                    `json:"duration_seconds"`
	Interval        int                    `json:"interval_seconds"`
	MetricsInterval int                    `json:"metrics_interval_seconds"`
	Compress        bool                   `json:"compress"`
	RawArgs         []string               `json:"raw_args"`
	Targets         []string               `json:"targets"`
	Output          map[string]interface{} `json:"output"`
	Errors          []*captureError        `json:"errors"`
}

// captureError hold an error entry that can occur during polling capture.
// It includes the timestamp, the target, and the error itself.
type captureError struct {
	TargetError string    `json:"error"`
	Target      string    `json:"target"`
	Timestamp   time.Time `json:"timestamp"`
}

// newCaptureError instantiates a new captureError.
func newCaptureError(target string, err error) *captureError {
	return &captureError{
		TargetError: err.Error(),
		Target:      target,
		Timestamp:   time.Now().UTC(),
	}
}

// serverStatus holds a single interval entry for the server-status target
type serverStatus struct {
	Timestamp time.Time               `json:"timestamp"`
	Health    *api.HealthResponse     `json:"health"`
	Seal      *api.SealStatusResponse `json:"seal"`
}

var _ cli.Command = (*DebugCommand)(nil)
var _ cli.CommandAutocomplete = (*DebugCommand)(nil)

type DebugCommand struct {
	*BaseCommand

	flagCompress        bool
	flagDuration        time.Duration
	flagInterval        time.Duration
	flagMetricsInterval time.Duration
	flagOutput          string
	flagTargets         []string

	// skipTimingChecks bypasses timing-related checks, used primarily for tests
	skipTimingChecks bool
	// logger is the logger used for outputting capture progress
	logger hclog.Logger

	// ShutdownCh is used to capture interrupt signal and end polling capture
	ShutdownCh chan struct{}
}

func (c *DebugCommand) AutocompleteArgs() complete.Predictor {
	// Predict targets
	return c.PredictVaultDebugTargets()
}

func (c *DebugCommand) AutocompleteFlags() complete.Flags {
	return c.Flags().Completions()
}

func (c *DebugCommand) Flags() *FlagSets {
	set := c.flagSet(FlagSetHTTP)

	f := set.NewFlagSet("Command Options")

	f.BoolVar(&BoolVar{
		Name:    "compress",
		Target:  &c.flagCompress,
		Default: true,
		Usage:   "Toggles whether to compress output package",
	})

	f.DurationVar(&DurationVar{
		Name:       "duration",
		Target:     &c.flagDuration,
		Completion: complete.PredictAnything,
		Default:    2 * time.Minute,
		Usage:      "Duration to run the command.",
	})

	f.DurationVar(&DurationVar{
		Name:       "interval",
		Target:     &c.flagInterval,
		Completion: complete.PredictAnything,
		Default:    30 * time.Second,
		Usage: "The interval in which to perform profiling and server state " +
			"capture, excluding metrics.",
	})

	f.DurationVar(&DurationVar{
		Name:       "metrics-interval",
		Target:     &c.flagMetricsInterval,
		Completion: complete.PredictAnything,
		Default:    10 * time.Second,
		Usage:      "The interval in which to perform metrics capture.",
	})

	f.StringVar(&StringVar{
		Name:       "output",
		Target:     &c.flagOutput,
		Completion: complete.PredictAnything,
		Usage:      "Specifies the output path for the debug package.",
	})

	f.StringSliceVar(&StringSliceVar{
		Name:   "targets",
		Target: &c.flagTargets,
		Usage: "Comma-separated string or list of targets to capture. Available " +
			"targets are: config, host, metrics, pprof, " +
			"replication-status, server-status.",
	})

	return set
}

func (c *DebugCommand) Help() string {
	helpText := `
Usage: vault debug [options]

  Probes a specific Vault server node for a specified period of time, recording
  information about the node, its cluster, and its host environment. The
  information collected is packaged and written to the specified path.

  Certain endpoints that this command issues require ACL permissions to access.
  If not permitted, the information from these endpoints will not be part of the
  output. The command uses the Vault address and token as specified via
  the login command, environment variables, or CLI flags.

  To create a debug package using default duration and interval values in the 
  current directory that captures all applicable targets:

  $ vault debug

  To create a debug package with a specific duration and interval in the current
  directory that capture all applicable targets:

  $ vault debug -duration=10m -interval=1m

  To create a debug package in the current directory with a specific sub-set of
  targets:

  $ vault debug -targets=host,metrics

` + c.Flags().Help()

	return helpText
}

func (c *DebugCommand) Run(args []string) int {
	f := c.Flags()

	if err := f.Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	parsedArgs := f.Args()
	if len(parsedArgs) > 0 {
		c.UI.Error(fmt.Sprintf("Too many arguments (expected 0, got %d)", len(parsedArgs)))
		return 1
	}

	// Initialize the logger for debug output
	logWriter := &gatedwriter.Writer{Writer: os.Stderr}
	if c.logger == nil {
		c.logger = logging.NewVaultLoggerWithWriter(logWriter, hclog.Trace)
	}

	client, debugIndex, dstOutputFile, err := c.preflight(args)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error during validation: %s", err))
		return 1
	}

	// Print debug information
	c.UI.Output("==> Starting debug capture...")
	c.UI.Info(fmt.Sprintf("         Vault Address: %s", debugIndex.VaultAddress))
	c.UI.Info(fmt.Sprintf("        Client Version: %s", debugIndex.ClientVersion))
	c.UI.Info(fmt.Sprintf("              Duration: %s", c.flagDuration))
	c.UI.Info(fmt.Sprintf("              Interval: %s", c.flagInterval))
	c.UI.Info(fmt.Sprintf("      Metrics Interval: %s", c.flagMetricsInterval))
	c.UI.Info(fmt.Sprintf("               Targets: %s", strings.Join(c.flagTargets, ", ")))
	c.UI.Info(fmt.Sprintf("                Output: %s", dstOutputFile))
	c.UI.Output("")

	// Release the log gate.
	logWriter.Flush()

	// Capture static information
	if err := c.captureStaticTargets(debugIndex); err != nil {
		c.UI.Error(fmt.Sprintf("Error capturing static information: %s", err))
		return 2
	}

	// Capture polling information
	if err := c.capturePollingTargets(debugIndex, client); err != nil {
		c.UI.Error(fmt.Sprintf("Error capturing dynamic information: %s", err))
		return 2
	}

	// Marshal and write index.js
	bytes, err := json.MarshalIndent(debugIndex, "", "  ")
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error marshalling index: %s", err))
		return 1
	}
	if err := ioutil.WriteFile(filepath.Join(c.flagOutput, "index.js"), bytes, 0644); err != nil {
		c.UI.Error(fmt.Sprintf("Unable to write index.js file: %s", err))
		return 1
	}

	if c.flagCompress {
		if err := c.compress(dstOutputFile); err != nil {
			c.UI.Error(fmt.Sprintf("Error encountered during bundle compression: %s", err))
			// We want to inform that data collection was captured and stored in
			// a directory even if compression fails
			c.UI.Info(fmt.Sprintf("Data written to: %s", c.flagOutput))
			return 1
		}
	}

	c.UI.Info(fmt.Sprintf("Success! Bundle written to: %s", dstOutputFile))
	return 0
}

func (c *DebugCommand) Synopsis() string {
	return "Runs the debug command"
}

// preflight performs various checks against the provided flags to ensure they
// are valid/reasonable values. It also takes care of instantiating a client and
// index object for use by the command.
func (c *DebugCommand) preflight(rawArgs []string) (*api.Client, *debugIndex, string, error) {
	if !c.skipTimingChecks {
		// Guard duration and interval values to acceptable values
		if c.flagDuration < debugMinInterval {
			c.UI.Info(fmt.Sprintf("Overwriting duration value %q to the minimum value of %q", c.flagDuration, debugMinInterval))
			c.flagDuration = debugMinInterval
		}
		if c.flagInterval < debugMinInterval {
			c.UI.Info(fmt.Sprintf("Overwriting inteval value %q to the minimum value of %q", c.flagInterval, debugMinInterval))
			c.flagInterval = debugMinInterval
		}
		if c.flagInterval > c.flagDuration {
			c.UI.Info(fmt.Sprintf("Overwriting inteval value %q to the duration value %q", c.flagInterval, c.flagDuration))
			c.flagInterval = c.flagDuration
		}
		if c.flagMetricsInterval < debugMinInterval {
			c.UI.Info(fmt.Sprintf("Overwriting metrics inteval value %q to the minimum value of %q", c.flagMetricsInterval, debugMinInterval))
			c.flagMetricsInterval = debugMinInterval
		}
		if c.flagMetricsInterval > c.flagDuration {
			c.UI.Info(fmt.Sprintf("Overwriting metrics inteval value %q to the duration value %q", c.flagMetricsInterval, c.flagDuration))
			c.flagMetricsInterval = c.flagDuration
		}
	}

	if len(c.flagTargets) == 0 {
		c.flagTargets = c.defaultTargets()
	}

	// Make sure we can talk to the server
	client, err := c.Client()
	if err != nil {
		return nil, nil, "", fmt.Errorf("unable to create client to connect to Vault: %s", err)
	}
	if _, err := client.Sys().Health(); err != nil {
		return nil, nil, "", fmt.Errorf("unable to connect to the server: %s", err)
	}

	captureTime := time.Now().UTC()
	if len(c.flagOutput) == 0 {
		formattedTime := captureTime.Format(fileFriendlyTimeFormat)
		c.flagOutput = fmt.Sprintf("vault-debug-%s", formattedTime)
	}

	// If compression is enabled, trim the extension so that the files are
	// written to a directory even if compression somehow fails. We ensure the
	// extension during compression. We also prevent overwriting if the file
	// already exists.
	dstOutputFile := c.flagOutput
	if c.flagCompress {
		if !strings.HasSuffix(dstOutputFile, ".tar.gz") && !strings.HasSuffix(dstOutputFile, ".tgz") {
			dstOutputFile = dstOutputFile + debugCompressionExt
		}

		// Ensure that the file doesn't already exist, and ensure that we always
		// trim the extension from flagOutput since we'll be progressively
		// writing to that.
		if _, err := os.Stat(dstOutputFile); os.IsNotExist(err) {
			c.flagOutput = strings.TrimSuffix(c.flagOutput, ".tar.gz")
			c.flagOutput = strings.TrimSuffix(c.flagOutput, ".tgz")
		} else {
			return nil, nil, "", fmt.Errorf("output file already exists: %s", dstOutputFile)
		}
	}

	// Stat check the directory to ensure we don't override any existing data.
	if _, err := os.Stat(c.flagOutput); os.IsNotExist(err) {
		err := os.MkdirAll(c.flagOutput, 0755)
		if err != nil {
			return nil, nil, "", fmt.Errorf("unable to create output directory: %s", err)
		}
	} else {
		return nil, nil, "", fmt.Errorf("output directory already exists: %s", c.flagOutput)
	}

	// Populate initial index fields
	idxOutput := map[string]interface{}{
		"files": []string{},
	}
	debugIndex := &debugIndex{
		VaultAddress:    client.Address(),
		ClientVersion:   version.GetVersion().VersionNumber(),
		Compress:        c.flagCompress,
		Duration:        int(c.flagDuration.Seconds()),
		Interval:        int(c.flagInterval.Seconds()),
		MetricsInterval: int(c.flagMetricsInterval.Seconds()),
		RawArgs:         rawArgs,
		Version:         debugIndexVersion,
		Targets:         c.flagTargets,
		Timestamp:       captureTime,
		Output:          idxOutput,
		Errors:          []*captureError{},
	}

	return client, debugIndex, dstOutputFile, nil
}

func (c *DebugCommand) defaultTargets() []string {
	return []string{"config", "metrics", "pprof", "replication-status", "server-status"}
}

func (c *DebugCommand) captureStaticTargets(index *debugIndex) error {
	c.UI.Info("==> Capturing static information...")
	// TODO: Perform config state capture
	c.logger.Info("capturing configuration state")
	c.UI.Output("")
	// Capture configuration state
	return nil
}

func (c *DebugCommand) capturePollingTargets(index *debugIndex, client *api.Client) error {
	startTime := time.Now()
	durationCh := time.After(c.flagDuration + debugDurationGrace)

	// Track current and total counts to show progress. We add one to the total
	// since we also capture the last tick on the total duration time frame
	// given a non-zero grace value.
	totalCount := int(c.flagDuration.Seconds()/c.flagInterval.Seconds()) + 1
	idxCount := 1

	mTotalCount := int(c.flagDuration.Seconds()/c.flagMetricsInterval.Seconds()) + 1
	mIdxCount := 1

	errCh := make(chan *captureError)
	defer close(errCh)

	var wg sync.WaitGroup
	// Profiling needs its own separate wait group since profile
	// and trace needs to be ran in a goroutine, but we want to
	// finish a capture before moving to the next one.
	var wgProf sync.WaitGroup

	var serverStatusCollection []*serverStatus
	var metricsCollection []map[string]interface{}

	intervalCapture := func() {
		currentTimestamp := time.Now().UTC()

		// Create a sub-directory for pprof data
		currentDir := currentTimestamp.Format(fileFriendlyTimeFormat)
		dirName := filepath.Join(c.flagOutput, currentDir)
		if err := os.MkdirAll(dirName, 0755); err != nil {
			c.UI.Error(fmt.Sprintf("Error creating sub-directory for time interval: %s", err))
			return
		}
		index.Output[currentDir] = map[string]interface{}{
			"timestamp": currentTimestamp,
			"files":     []string{},
		}

		if strutil.StrListContains(c.flagTargets, "config") {

		}

		if strutil.StrListContains(c.flagTargets, "pprof") {
			c.logger.Info("capturing pprof data", "current", idxCount, "total", totalCount)

			wg.Add(1)
			go func() {
				defer wg.Done()

				// Wait for other profiling requests to finish before starting this one.
				wgProf.Wait()

				// Capture goroutines
				data, err := pprofGoroutine(client)
				if err != nil {
					errCh <- newCaptureError("pprof.goroutine", err)
				}

				err = ioutil.WriteFile(filepath.Join(dirName, "goroutine.prof"), data, 0644)
				if err != nil {
					errCh <- newCaptureError("pprof.goroutine", err)
				}
				// Add file to the index
				filesArr := index.Output[currentDir].(map[string]interface{})["files"]
				index.Output[currentDir].(map[string]interface{})["files"] = append(filesArr.([]string), "goroutine.prof")

				// Capture heap
				data, err = pprofHeap(client)
				if err != nil {
					errCh <- newCaptureError("pprof.heap", err)
				}

				err = ioutil.WriteFile(filepath.Join(dirName, "heap.prof"), data, 0644)
				if err != nil {
					errCh <- newCaptureError("pprof.heap", err)
				}
				// Add file to the index
				filesArr = index.Output[currentDir].(map[string]interface{})["files"]
				index.Output[currentDir].(map[string]interface{})["files"] = append(filesArr.([]string), "heap.prof")

				// If the our remaining duration is less than the interval value
				// skip profile and trace.
				runDuration := currentTimestamp.Sub(startTime)
				if (c.flagDuration+debugDurationGrace)-runDuration < c.flagInterval {
					return
				}

				// Capture profile
				wgProf.Add(1)
				go func() {
					defer wgProf.Done()
					data, err := pprofProfile(client, c.flagInterval)
					if err != nil {
						errCh <- newCaptureError("pprof.profile", err)
						return
					}

					err = ioutil.WriteFile(filepath.Join(dirName, "profile.prof"), data, 0644)
					if err != nil {
						errCh <- newCaptureError("pprof.profile", err)
					}
					filesArr = index.Output[currentDir].(map[string]interface{})["files"]
					index.Output[currentDir].(map[string]interface{})["files"] = append(filesArr.([]string), "profile.prof")

				}()

				// Capture trace
				wgProf.Add(1)
				go func() {
					defer wgProf.Done()
					data, err := pprofTrace(client, c.flagInterval)
					if err != nil {
						errCh <- newCaptureError("pprof.trace", err)
						return
					}

					err = ioutil.WriteFile(filepath.Join(dirName, "trace.out"), data, 0644)
					if err != nil {
						errCh <- newCaptureError("pprof.trace", err)
					}
					filesArr = index.Output[currentDir].(map[string]interface{})["files"]
					index.Output[currentDir].(map[string]interface{})["files"] = append(filesArr.([]string), "trace.out")

				}()
				wgProf.Wait()
			}()
		}

		if strutil.StrListContains(c.flagTargets, "replication-status") {

		}

		if strutil.StrListContains(c.flagTargets, "server-status") {
			c.logger.Info("capturing server status information", "current", idxCount, "total", totalCount)

			wg.Add(1)
			go func() {
				// Naive approach for now, but we shouldn't have to hold things
				// inmem until the end since we're appending to a file. The
				// challenge is figuring out how to return as a single
				// array of objects so that it's valid JSON.
				healthInfo, err := client.Sys().Health()
				if err != nil {
					errCh <- newCaptureError("server-status.health", err)
				}
				sealInfo, err := client.Sys().SealStatus()
				if err != nil {
					errCh <- newCaptureError("server-status.seal", err)
				}

				entry := &serverStatus{
					Timestamp: currentTimestamp,
					Health:    healthInfo,
					Seal:      sealInfo,
				}
				serverStatusCollection = append(serverStatusCollection, entry)

				wg.Done()
			}()
		}
		wg.Wait()
	}

	metricsIntervalCapture := func() {
		if strutil.StrListContains(c.flagTargets, "metrics") {
			c.logger.Info("capturing metrics", "current", mIdxCount, "total", mTotalCount)

			healthStatus, err := client.Sys().Health()
			if err != nil {
				errCh <- newCaptureError("metrics", err)
				return
			}

			// Check replication status. We skip on process metrics if we're one
			// of the following (since the request will be forwarded):
			// 1. Any type of DR Node
			// 2. Non-DR, non-performance standby nodes
			switch {
			case healthStatus.ReplicationDRMode == "secondary":
				c.logger.Info("skipping metrics capture on DR secondary node")
				return
			case healthStatus.Standby && !healthStatus.PerformanceStandby:
				c.logger.Info("skipping metrics on standby node")
				return
			}

			wg.Add(1)
			go func() {
				r := client.NewRequest("GET", "/v1/sys/metrics")

				metricsResp, err := client.RawRequest(r)
				if err != nil {
					errCh <- newCaptureError("metrics", err)
				}
				if metricsResp != nil {
					defer metricsResp.Body.Close()

					metricsEntry := make(map[string]interface{})
					err := json.NewDecoder(metricsResp.Body).Decode(&metricsEntry)
					if err != nil {
						errCh <- newCaptureError("metrics", err)
					}
					metricsCollection = append(metricsCollection, metricsEntry)
				}

				wg.Done()
			}()
		}
		wg.Wait()
	}

	// Upon exit write the targets that we've collection its respective files
	// and update the index.
	defer func() {
		metricsBytes, err := json.MarshalIndent(metricsCollection, "", "  ")
		if err != nil {
			c.UI.Error("Error marshaling metrics.json data")
			return
		}
		if err := ioutil.WriteFile(filepath.Join(c.flagOutput, "metrics.json"), metricsBytes, 0644); err != nil {
			c.UI.Error("Error writing data to metrics.json")
			return
		}
		index.Output["files"] = append(index.Output["files"].([]string), "metrics.json")

		serverStatusBytes, err := json.MarshalIndent(serverStatusCollection, "", "  ")
		if err != nil {
			c.UI.Error("Error marshaling server-status.json data")
			return
		}
		if err := ioutil.WriteFile(filepath.Join(c.flagOutput, "server-status.json"), serverStatusBytes, 0644); err != nil {
			c.UI.Error("Error writing data to server-status.json")
			return
		}
		index.Output["files"] = append(index.Output["files"].([]string), "server-status.json")
	}()

	// Start capture by capturing the first interval before we hit the first
	// ticker.
	c.UI.Info("==> Capturing dynamic information...")
	go intervalCapture()
	go metricsIntervalCapture()

	// Capture at each interval, until end of duration or interrupt.
	intervalTicker := time.Tick(c.flagInterval)
	metricsIntervalTicker := time.Tick(c.flagMetricsInterval)
	for {
		select {
		case err := <-errCh:
			index.Errors = append(index.Errors, err)
		case <-intervalTicker:
			idxCount++
			go intervalCapture()
		case <-metricsIntervalTicker:
			mIdxCount++
			go metricsIntervalCapture()
		case <-durationCh:
			return nil
		case <-c.ShutdownCh:
			c.UI.Info("Caught interrupt signal, exiting...")
			return nil
		}
	}
}

func (c *DebugCommand) compress(dst string) error {
	tgz := archiver.NewTarGz()
	if err := tgz.Archive([]string{c.flagOutput}, dst); err != nil {
		return fmt.Errorf("failed to compress data: %s", err)
	}

	// If everything is fine up to this point, remove original directory
	if err := os.RemoveAll(c.flagOutput); err != nil {
		return fmt.Errorf("failed to remove data directory: %s", err)
	}

	return nil
}

func pprofGoroutine(client *api.Client) ([]byte, error) {
	req := client.NewRequest("GET", "/v1/sys/pprof/goroutine")
	resp, err := client.RawRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func pprofHeap(client *api.Client) ([]byte, error) {
	req := client.NewRequest("GET", "/v1/sys/pprof/heap")
	resp, err := client.RawRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func pprofProfile(client *api.Client, duration time.Duration) ([]byte, error) {
	seconds := int(duration.Seconds())
	secStr := strconv.Itoa(seconds)

	req := client.NewRequest("GET", "/v1/sys/pprof/profile")
	req.Params.Add("seconds", secStr)
	resp, err := client.RawRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func pprofTrace(client *api.Client, duration time.Duration) ([]byte, error) {
	seconds := int(duration.Seconds())
	secStr := strconv.Itoa(seconds)

	req := client.NewRequest("GET", "/v1/sys/pprof/trace")
	req.Params.Add("seconds", secStr)
	resp, err := client.RawRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return data, nil
}
