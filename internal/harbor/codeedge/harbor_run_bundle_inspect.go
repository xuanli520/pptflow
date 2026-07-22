package codeedge

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const harborRunBundleExpectedTrialCount = 4

var harborRunBundleNaiveJobTimestampV018 = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?$`)

// HarborRunBundleJobFactsV018 is the minimal typed projection required by the
// 0.18 evaluator adapter. PassAtK is grouped exactly as Harbor's job summary
// writes it: evaluator group -> string k -> value. InternalRetryCount is
// Harbor's authenticated job-wide stats.n_retries aggregate. Harbor 0.18 does
// not expose a verified mapping from that aggregate to an individual final
// Trial result, so it must never be expanded into inferred TrialAttempt facts.
type HarborRunBundleJobFactsV018 struct {
	ID string `json:"id"`
	// FinishedAt is the exact job-level timestamp emitted by Harbor. Harbor
	// 0.18 may emit a naive Python datetime here, so this field is deliberately
	// not a time.Time and must not be treated as a UTC instant.
	FinishedAt         string                        `json:"finished_at"`
	TotalTrials        int                           `json:"n_total_trials"`
	RunningTrials      int                           `json:"n_running_trials"`
	PendingTrials      int                           `json:"n_pending_trials"`
	InternalRetryCount int                           `json:"n_retries"`
	PassAtK            map[string]map[string]float64 `json:"pass_at_k"`
}

// HarborRunBundleEvaluatorFactsV018 reflects Harbor's TrialResult agent_info.
// ModelName and ModelProvider are pointers so absence remains an observed fact;
// a missing provider is never guessed or replaced with a default.
type HarborRunBundleEvaluatorFactsV018 struct {
	AgentName     string  `json:"agent_name"`
	AgentVersion  string  `json:"agent_version"`
	ModelName     *string `json:"model_name,omitempty"`
	ModelProvider *string `json:"model_provider,omitempty"`
}

// HarborRunBundleTrialFactsV018 keeps Harbor's three independent task
// identities separate. TaskChecksum is TrialResult.task_checksum (dirhash in
// the observed Harbor source); LockTaskDigest is TrialLock.task.digest; neither
// is equated with the managed V2 task digest stored in the enclosing bundle.
type HarborRunBundleTrialFactsV018 struct {
	ID                   string                            `json:"id"`
	Name                 string                            `json:"name"`
	Directory            string                            `json:"directory"`
	JobID                string                            `json:"job_id"`
	TaskChecksum         string                            `json:"task_checksum"`
	LockTaskDigest       string                            `json:"lock_task_digest"`
	StartedAt            time.Time                         `json:"started_at"`
	FinishedAt           time.Time                         `json:"finished_at"`
	Elapsed              time.Duration                     `json:"elapsed"`
	ExceptionType        string                            `json:"exception_type,omitempty"`
	Evaluator            HarborRunBundleEvaluatorFactsV018 `json:"evaluator"`
	VerifierRewards      map[string]float64                `json:"verifier_rewards"`
	TrajectoryTotalSteps *int                              `json:"trajectory_total_steps,omitempty"`
}

// HarborRunBundleInspectionV018 is a verified, in-memory reader over one
// canonical bundle. The underlying map is private so callers cannot mutate
// captured bytes or alter an already inspected set of facts.
type HarborRunBundleInspectionV018 struct {
	bundle     HarborRunBundleV018
	files      map[string][]byte
	job        HarborRunBundleJobFactsV018
	trials     []HarborRunBundleTrialFactsV018
	trialsByID map[string]HarborRunBundleTrialFactsV018
}

// InspectHarborRunBundleV018 validates the canonical bundle's concrete Harbor
// layout and returns raw-file readers plus the fields consumed by evaluation.
// It requires exactly four independent top-level trial directories, each with
// config.json, lock.json, and result.json. agent/trajectory.json is optional at
// this transport layer because not every Harbor agent emits one; when present,
// its final_metrics.total_steps is strict and exposed.
func InspectHarborRunBundleV018(bundle HarborRunBundleV018) (*HarborRunBundleInspectionV018, error) {
	if err := bundle.validate(); err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(bundle.Files))
	for _, file := range bundle.Files {
		content, err := harborRunBundleDecodeFile(file)
		if err != nil {
			return nil, err
		}
		files[file.Path] = content
	}
	jobConfig, err := harborRunBundleRequiredFile(files, "config.json")
	if err != nil {
		return nil, err
	}
	if err := harborRunBundleValidatePassFourJobConfig(jobConfig); err != nil {
		return nil, err
	}
	jobLock, err := harborRunBundleRequiredFile(files, "lock.json")
	if err != nil {
		return nil, err
	}
	if err := harborRunBundleValidateJobLock(jobLock, bundle.HarborCLI.Version); err != nil {
		return nil, err
	}
	jobRaw, err := harborRunBundleRequiredFile(files, "result.json")
	if err != nil {
		return nil, err
	}
	job, err := harborRunBundleParseJobFacts(jobRaw)
	if err != nil {
		return nil, err
	}
	if job.TotalTrials != harborRunBundleExpectedTrialCount {
		return nil, fmt.Errorf("%w: Harbor job n_total_trials = %d, want %d", ErrInvalidHarborRunBundle, job.TotalTrials, harborRunBundleExpectedTrialCount)
	}

	directories, err := harborRunBundleTrialDirectories(files)
	if err != nil {
		return nil, err
	}
	if len(directories) != harborRunBundleExpectedTrialCount {
		return nil, fmt.Errorf("%w: found %d trial result directories, want %d", ErrInvalidHarborRunBundle, len(directories), harborRunBundleExpectedTrialCount)
	}
	trials := make([]HarborRunBundleTrialFactsV018, 0, len(directories))
	byID := make(map[string]HarborRunBundleTrialFactsV018, len(directories))
	seenNames := make(map[string]struct{}, len(directories))
	for _, directory := range directories {
		trial, err := harborRunBundleParseTrialFacts(files, directory, job.ID)
		if err != nil {
			return nil, err
		}
		if _, duplicate := byID[trial.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate Harbor trial id %q", ErrInvalidHarborRunBundle, trial.ID)
		}
		if _, duplicate := seenNames[trial.Name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate Harbor trial name %q", ErrInvalidHarborRunBundle, trial.Name)
		}
		byID[trial.ID] = trial
		seenNames[trial.Name] = struct{}{}
		trials = append(trials, trial)
	}
	sort.Slice(trials, func(left, right int) bool {
		if trials[left].Name != trials[right].Name {
			return trials[left].Name < trials[right].Name
		}
		return trials[left].ID < trials[right].ID
	})
	return &HarborRunBundleInspectionV018{
		bundle: bundle.clone(), files: files, job: cloneHarborRunBundleJobFacts(job), trials: cloneHarborRunBundleTrials(trials), trialsByID: byID,
	}, nil
}

func harborRunBundleValidatePassFourJobConfig(raw []byte) error {
	root, err := harborRunBundleJSONObject(raw, "Harbor job config")
	if err != nil {
		return err
	}
	attempts, err := harborRunBundleRequiredNonNegativeInt(root, "n_attempts", "Harbor job config")
	if err != nil {
		return err
	}
	if attempts != harborRunBundleExpectedTrialCount {
		return fmt.Errorf("%w: Harbor job n_attempts = %d, want %d", ErrInvalidHarborRunBundle, attempts, harborRunBundleExpectedTrialCount)
	}
	tasks, err := harborRunBundleRequiredArray(root, "tasks", "Harbor job config")
	if err != nil || len(tasks) != 1 {
		if err == nil {
			err = errors.New("must contain exactly one task")
		}
		return fmt.Errorf("%w: Harbor job config.tasks: %v", ErrInvalidHarborRunBundle, err)
	}
	task, err := harborRunBundleJSONObject(tasks[0], "Harbor job config.tasks[0]")
	if err != nil {
		return err
	}
	if _, err := harborRunBundleRequiredString(task, "path", "Harbor job config.tasks[0]"); err != nil {
		return err
	}
	agents, err := harborRunBundleRequiredArray(root, "agents", "Harbor job config")
	if err != nil || len(agents) != 1 {
		if err == nil {
			err = errors.New("must contain exactly one evaluator entry")
		}
		return fmt.Errorf("%w: Harbor job config.agents: %v", ErrInvalidHarborRunBundle, err)
	}
	if _, err := harborRunBundleJSONObject(agents[0], "Harbor job config.agents[0]"); err != nil {
		return err
	}
	if rawDatasets, present := root["datasets"]; present && !harborRunBundleJSONNull(rawDatasets) {
		datasets, parseErr := harborRunBundleArray(rawDatasets, "Harbor job config.datasets")
		if parseErr != nil || len(datasets) != 0 {
			if parseErr == nil {
				parseErr = errors.New("must be empty for one controlled local task")
			}
			return fmt.Errorf("%w: Harbor job config.datasets: %v", ErrInvalidHarborRunBundle, parseErr)
		}
	}
	return nil
}

func harborRunBundleValidateJobLock(raw []byte, expectedHarborVersion string) error {
	root, err := harborRunBundleJSONObject(raw, "Harbor job lock")
	if err != nil {
		return err
	}
	schemaVersion, err := harborRunBundleRequiredNonNegativeInt(root, "schema_version", "Harbor job lock")
	if err != nil {
		return err
	}
	if schemaVersion != 2 {
		return fmt.Errorf("%w: Harbor job lock schema_version = %d, want 2", ErrInvalidHarborRunBundle, schemaVersion)
	}
	harbor, err := harborRunBundleRequiredObject(root, "harbor", "Harbor job lock")
	if err != nil {
		return err
	}
	version, err := harborRunBundleRequiredString(harbor, "version", "Harbor job lock.harbor")
	if err != nil {
		return err
	}
	if version != expectedHarborVersion {
		return fmt.Errorf("%w: Harbor job lock version %q does not match frozen CLI version %q", ErrInvalidHarborRunBundle, version, expectedHarborVersion)
	}
	return nil
}

// ParseAndInspectHarborRunBundleV018 is the safe one-step reader for raw
// stored evidence bytes.
func ParseAndInspectHarborRunBundleV018(raw []byte) (*HarborRunBundleInspectionV018, error) {
	bundle, err := ParseHarborRunBundleV018(raw)
	if err != nil {
		return nil, err
	}
	return InspectHarborRunBundleV018(bundle)
}

// Bundle returns an independently owned bundle copy.
func (inspection *HarborRunBundleInspectionV018) Bundle() HarborRunBundleV018 {
	if inspection == nil {
		return HarborRunBundleV018{}
	}
	return inspection.bundle.clone()
}

// Job returns a copy of the typed job facts.
func (inspection *HarborRunBundleInspectionV018) Job() HarborRunBundleJobFactsV018 {
	if inspection == nil {
		return HarborRunBundleJobFactsV018{}
	}
	return cloneHarborRunBundleJobFacts(inspection.job)
}

// Trials returns the four verified logical trial facts in stable name/id order.
func (inspection *HarborRunBundleInspectionV018) Trials() []HarborRunBundleTrialFactsV018 {
	if inspection == nil {
		return nil
	}
	return cloneHarborRunBundleTrials(inspection.trials)
}

// JobConfigJSON returns the captured top-level Harbor config.json bytes.
func (inspection *HarborRunBundleInspectionV018) JobConfigJSON() ([]byte, error) {
	return inspection.file("config.json")
}

// JobLockJSON returns the captured top-level Harbor lock.json bytes.
func (inspection *HarborRunBundleInspectionV018) JobLockJSON() ([]byte, error) {
	return inspection.file("lock.json")
}

// JobResultJSON returns the captured top-level Harbor result.json bytes.
func (inspection *HarborRunBundleInspectionV018) JobResultJSON() ([]byte, error) {
	return inspection.file("result.json")
}

// TrialConfigJSON returns one captured per-trial config.json by Harbor trial ID.
func (inspection *HarborRunBundleInspectionV018) TrialConfigJSON(trialID string) ([]byte, error) {
	trial, err := inspection.trial(trialID)
	if err != nil {
		return nil, err
	}
	return inspection.file(path.Join(trial.Directory, "config.json"))
}

// TrialLockJSON returns one captured per-trial lock.json by Harbor trial ID.
func (inspection *HarborRunBundleInspectionV018) TrialLockJSON(trialID string) ([]byte, error) {
	trial, err := inspection.trial(trialID)
	if err != nil {
		return nil, err
	}
	return inspection.file(path.Join(trial.Directory, "lock.json"))
}

// TrialResultJSON returns one captured per-trial result.json by Harbor trial ID.
func (inspection *HarborRunBundleInspectionV018) TrialResultJSON(trialID string) ([]byte, error) {
	trial, err := inspection.trial(trialID)
	if err != nil {
		return nil, err
	}
	return inspection.file(path.Join(trial.Directory, "result.json"))
}

// TrialTrajectoryJSON returns agent/trajectory.json when Harbor produced it.
// The bool is false when the capture contains no trajectory for that Trial.
func (inspection *HarborRunBundleInspectionV018) TrialTrajectoryJSON(trialID string) ([]byte, bool, error) {
	trial, err := inspection.trial(trialID)
	if err != nil {
		return nil, false, err
	}
	raw, found := inspection.files[path.Join(trial.Directory, "agent", "trajectory.json")]
	if !found {
		return nil, false, nil
	}
	return append([]byte(nil), raw...), true, nil
}

func (inspection *HarborRunBundleInspectionV018) file(bundlePath string) ([]byte, error) {
	if inspection == nil {
		return nil, fmt.Errorf("%w: nil inspection", ErrInvalidHarborRunBundle)
	}
	raw, found := inspection.files[bundlePath]
	if !found {
		return nil, fmt.Errorf("%w: required captured file %q is absent", ErrInvalidHarborRunBundle, bundlePath)
	}
	return append([]byte(nil), raw...), nil
}

func (inspection *HarborRunBundleInspectionV018) trial(id string) (HarborRunBundleTrialFactsV018, error) {
	if inspection == nil {
		return HarborRunBundleTrialFactsV018{}, fmt.Errorf("%w: nil inspection", ErrInvalidHarborRunBundle)
	}
	trial, found := inspection.trialsByID[strings.TrimSpace(id)]
	if !found {
		return HarborRunBundleTrialFactsV018{}, fmt.Errorf("%w: Harbor trial %q is absent", ErrInvalidHarborRunBundle, id)
	}
	return cloneHarborRunBundleTrialFacts(trial), nil
}

func harborRunBundleDecodeFile(file HarborRunBundleFileV018) ([]byte, error) {
	for _, character := range file.ContentBase64 {
		if character > 0x7f {
			return nil, fmt.Errorf("%w: file %q has non-ASCII base64", ErrInvalidHarborRunBundle, file.Path)
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(file.ContentBase64)
	if err != nil {
		return nil, fmt.Errorf("%w: decode file %q: %v", ErrInvalidHarborRunBundle, file.Path, err)
	}
	return decoded, nil
}

func harborRunBundleRequiredFile(files map[string][]byte, bundlePath string) ([]byte, error) {
	raw, found := files[bundlePath]
	if !found {
		return nil, fmt.Errorf("%w: required captured file %q is absent", ErrInvalidHarborRunBundle, bundlePath)
	}
	return raw, nil
}

func harborRunBundleTrialDirectories(files map[string][]byte) ([]string, error) {
	directories := make(map[string]struct{})
	for bundlePath := range files {
		if path.Base(bundlePath) != "result.json" {
			continue
		}
		directory := path.Dir(bundlePath)
		if directory == "." || strings.Contains(directory, "/") {
			continue
		}
		if _, configFound := files[path.Join(directory, "config.json")]; !configFound {
			return nil, fmt.Errorf("%w: trial directory %q has result.json but no config.json", ErrInvalidHarborRunBundle, directory)
		}
		if _, lockFound := files[path.Join(directory, "lock.json")]; !lockFound {
			return nil, fmt.Errorf("%w: trial directory %q has result.json but no lock.json", ErrInvalidHarborRunBundle, directory)
		}
		directories[directory] = struct{}{}
	}
	result := make([]string, 0, len(directories))
	for directory := range directories {
		result = append(result, directory)
	}
	sort.Strings(result)
	return result, nil
}

func harborRunBundleParseJobFacts(raw []byte) (HarborRunBundleJobFactsV018, error) {
	root, err := harborRunBundleJSONObject(raw, "Harbor job result")
	if err != nil {
		return HarborRunBundleJobFactsV018{}, err
	}
	id, err := harborRunBundleRequiredString(root, "id", "Harbor job result")
	if err != nil {
		return HarborRunBundleJobFactsV018{}, err
	}
	total, err := harborRunBundleRequiredNonNegativeInt(root, "n_total_trials", "Harbor job result")
	if err != nil {
		return HarborRunBundleJobFactsV018{}, err
	}
	finishedAt, err := harborRunBundleRequiredJobFinishedAtV018(root, "finished_at", "Harbor job result")
	if err != nil {
		return HarborRunBundleJobFactsV018{}, err
	}
	stats, err := harborRunBundleRequiredObject(root, "stats", "Harbor job result")
	if err != nil {
		return HarborRunBundleJobFactsV018{}, err
	}
	running, err := harborRunBundleRequiredNonNegativeInt(stats, "n_running_trials", "Harbor job result.stats")
	if err != nil {
		return HarborRunBundleJobFactsV018{}, err
	}
	pending, err := harborRunBundleRequiredNonNegativeInt(stats, "n_pending_trials", "Harbor job result.stats")
	if err != nil {
		return HarborRunBundleJobFactsV018{}, err
	}
	internalRetries, err := harborRunBundleRequiredNonNegativeInt(stats, "n_retries", "Harbor job result.stats")
	if err != nil {
		return HarborRunBundleJobFactsV018{}, err
	}
	passAtK, err := harborRunBundleParsePassAtK(stats)
	if err != nil {
		return HarborRunBundleJobFactsV018{}, err
	}
	return HarborRunBundleJobFactsV018{
		ID: id, FinishedAt: finishedAt, TotalTrials: total, RunningTrials: running, PendingTrials: pending,
		InternalRetryCount: internalRetries, PassAtK: passAtK,
	}, nil
}

func harborRunBundleParsePassAtK(stats map[string]json.RawMessage) (map[string]map[string]float64, error) {
	rawEvals, present := stats["evals"]
	if !present || harborRunBundleJSONNull(rawEvals) {
		return map[string]map[string]float64{}, nil
	}
	evals, err := harborRunBundleJSONObject(rawEvals, "Harbor job result.stats.evals")
	if err != nil {
		return nil, err
	}
	result := make(map[string]map[string]float64, len(evals))
	for group, rawGroup := range evals {
		if strings.TrimSpace(group) == "" {
			return nil, fmt.Errorf("%w: Harbor pass@k evaluator group is empty", ErrInvalidHarborRunBundle)
		}
		entry, err := harborRunBundleJSONObject(rawGroup, "Harbor job result.stats.evals."+group)
		if err != nil {
			return nil, err
		}
		rawPassAtK, present := entry["pass_at_k"]
		if !present || harborRunBundleJSONNull(rawPassAtK) {
			result[group] = map[string]float64{}
			continue
		}
		values, err := harborRunBundleJSONObject(rawPassAtK, "Harbor job result.stats.evals."+group+".pass_at_k")
		if err != nil {
			return nil, err
		}
		groupValues := make(map[string]float64, len(values))
		for k, rawValue := range values {
			value, numberErr := harborRunBundleNumber(rawValue, "Harbor pass@k "+k)
			if numberErr != nil {
				return nil, numberErr
			}
			groupValues[k] = value
		}
		result[group] = groupValues
	}
	return result, nil
}

func harborRunBundleParseTrialFacts(files map[string][]byte, directory, expectedJobID string) (HarborRunBundleTrialFactsV018, error) {
	configRaw, err := harborRunBundleRequiredFile(files, path.Join(directory, "config.json"))
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	config, err := harborRunBundleJSONObject(configRaw, "Harbor trial config "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	configJobID, err := harborRunBundleRequiredString(config, "job_id", "Harbor trial config "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	lockRaw, err := harborRunBundleRequiredFile(files, path.Join(directory, "lock.json"))
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	lock, err := harborRunBundleJSONObject(lockRaw, "Harbor trial lock "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	taskLock, err := harborRunBundleRequiredObject(lock, "task", "Harbor trial lock "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	lockDigest, err := harborRunBundleRequiredString(taskLock, "digest", "Harbor trial lock "+directory+".task")
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	if err := workflowkit.Fingerprint(lockDigest).Validate(); err != nil {
		return HarborRunBundleTrialFactsV018{}, fmt.Errorf("%w: Harbor trial lock task.digest: %v", ErrInvalidHarborRunBundle, err)
	}

	resultRaw, err := harborRunBundleRequiredFile(files, path.Join(directory, "result.json"))
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	result, err := harborRunBundleJSONObject(resultRaw, "Harbor trial result "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	id, err := harborRunBundleRequiredString(result, "id", "Harbor trial result "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	name, err := harborRunBundleRequiredString(result, "trial_name", "Harbor trial result "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	checksum, err := harborRunBundleRequiredString(result, "task_checksum", "Harbor trial result "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	resultConfig, err := harborRunBundleRequiredObject(result, "config", "Harbor trial result "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	resultJobID, err := harborRunBundleRequiredString(resultConfig, "job_id", "Harbor trial result "+directory+".config")
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	if configJobID != expectedJobID || resultJobID != expectedJobID || configJobID != resultJobID {
		return HarborRunBundleTrialFactsV018{}, fmt.Errorf("%w: Harbor trial %q does not bind to job %q", ErrInvalidHarborRunBundle, directory, expectedJobID)
	}
	startedAt, err := harborRunBundleRequiredRFC3339Time(result, "started_at", "Harbor trial result "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	finishedAt, err := harborRunBundleRequiredRFC3339Time(result, "finished_at", "Harbor trial result "+directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	if finishedAt.Before(startedAt) {
		return HarborRunBundleTrialFactsV018{}, fmt.Errorf("%w: Harbor trial %q finished before it started", ErrInvalidHarborRunBundle, directory)
	}
	exceptionType, err := harborRunBundleExceptionType(result, directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	evaluator, err := harborRunBundleParseEvaluator(result, directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	rewards, err := harborRunBundleParseRewards(result, directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	trajectorySteps, err := harborRunBundleTrajectorySteps(files, directory)
	if err != nil {
		return HarborRunBundleTrialFactsV018{}, err
	}
	return HarborRunBundleTrialFactsV018{
		ID: id, Name: name, Directory: directory, JobID: expectedJobID, TaskChecksum: checksum, LockTaskDigest: lockDigest,
		StartedAt: startedAt, FinishedAt: finishedAt, Elapsed: finishedAt.Sub(startedAt), ExceptionType: exceptionType,
		Evaluator: evaluator, VerifierRewards: rewards, TrajectoryTotalSteps: trajectorySteps,
	}, nil
}

func harborRunBundleExceptionType(result map[string]json.RawMessage, directory string) (string, error) {
	rawException, present := result["exception_info"]
	if !present || harborRunBundleJSONNull(rawException) {
		return "", nil
	}
	exception, err := harborRunBundleJSONObject(rawException, "Harbor trial result "+directory+".exception_info")
	if err != nil {
		return "", err
	}
	return harborRunBundleRequiredString(exception, "exception_type", "Harbor trial result "+directory+".exception_info")
}

func harborRunBundleParseEvaluator(result map[string]json.RawMessage, directory string) (HarborRunBundleEvaluatorFactsV018, error) {
	agent, err := harborRunBundleRequiredObject(result, "agent_info", "Harbor trial result "+directory)
	if err != nil {
		return HarborRunBundleEvaluatorFactsV018{}, err
	}
	name, err := harborRunBundleRequiredString(agent, "name", "Harbor trial result "+directory+".agent_info")
	if err != nil {
		return HarborRunBundleEvaluatorFactsV018{}, err
	}
	version, err := harborRunBundleRequiredString(agent, "version", "Harbor trial result "+directory+".agent_info")
	if err != nil {
		return HarborRunBundleEvaluatorFactsV018{}, err
	}
	facts := HarborRunBundleEvaluatorFactsV018{AgentName: name, AgentVersion: version}
	rawModel, present := agent["model_info"]
	if !present || harborRunBundleJSONNull(rawModel) {
		return facts, nil
	}
	model, err := harborRunBundleJSONObject(rawModel, "Harbor trial result "+directory+".agent_info.model_info")
	if err != nil {
		return HarborRunBundleEvaluatorFactsV018{}, err
	}
	modelName, err := harborRunBundleRequiredString(model, "name", "Harbor trial result "+directory+".agent_info.model_info")
	if err != nil {
		return HarborRunBundleEvaluatorFactsV018{}, err
	}
	facts.ModelName = &modelName
	provider, err := harborRunBundleOptionalString(model, "provider", "Harbor trial result "+directory+".agent_info.model_info")
	if err != nil {
		return HarborRunBundleEvaluatorFactsV018{}, err
	}
	if provider != "" {
		facts.ModelProvider = &provider
	}
	return facts, nil
}

func harborRunBundleParseRewards(result map[string]json.RawMessage, directory string) (map[string]float64, error) {
	rawVerifier, present := result["verifier_result"]
	if !present || harborRunBundleJSONNull(rawVerifier) {
		return map[string]float64{}, nil
	}
	verifier, err := harborRunBundleJSONObject(rawVerifier, "Harbor trial result "+directory+".verifier_result")
	if err != nil {
		return nil, err
	}
	rawRewards, present := verifier["rewards"]
	if !present || harborRunBundleJSONNull(rawRewards) {
		return map[string]float64{}, nil
	}
	rewards, err := harborRunBundleJSONObject(rawRewards, "Harbor trial result "+directory+".verifier_result.rewards")
	if err != nil {
		return nil, err
	}
	resultRewards := make(map[string]float64, len(rewards))
	for key, rawValue := range rewards {
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("%w: Harbor verifier reward key is empty", ErrInvalidHarborRunBundle)
		}
		value, numberErr := harborRunBundleNumber(rawValue, "Harbor verifier reward "+key)
		if numberErr != nil {
			return nil, numberErr
		}
		resultRewards[key] = value
	}
	return resultRewards, nil
}

func harborRunBundleTrajectorySteps(files map[string][]byte, directory string) (*int, error) {
	raw, found := files[path.Join(directory, "agent", "trajectory.json")]
	if !found {
		return nil, nil
	}
	trajectory, err := harborRunBundleJSONObject(raw, "Harbor trial trajectory "+directory)
	if err != nil {
		return nil, err
	}
	metrics, err := harborRunBundleRequiredObject(trajectory, "final_metrics", "Harbor trial trajectory "+directory)
	if err != nil {
		return nil, err
	}
	steps, err := harborRunBundleRequiredNonNegativeInt(metrics, "total_steps", "Harbor trial trajectory "+directory+".final_metrics")
	if err != nil {
		return nil, err
	}
	return &steps, nil
}

func harborRunBundleRequiredObject(object map[string]json.RawMessage, key, label string) (map[string]json.RawMessage, error) {
	raw, present := object[key]
	if !present {
		return nil, fmt.Errorf("%w: %s.%s is required", ErrInvalidHarborRunBundle, label, key)
	}
	return harborRunBundleJSONObject(raw, label+"."+key)
}

func harborRunBundleOptionalString(object map[string]json.RawMessage, key, label string) (string, error) {
	raw, present := object[key]
	if !present || harborRunBundleJSONNull(raw) {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: %s.%s must be a string or null", ErrInvalidHarborRunBundle, label, key)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: %s.%s must not be empty when present", ErrInvalidHarborRunBundle, label, key)
	}
	return value, nil
}

func harborRunBundleRequiredNonNegativeInt(object map[string]json.RawMessage, key, label string) (int, error) {
	raw, present := object[key]
	if !present || harborRunBundleJSONNull(raw) {
		return 0, fmt.Errorf("%w: %s.%s is required", ErrInvalidHarborRunBundle, label, key)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return 0, fmt.Errorf("%w: %s.%s must be an integer", ErrInvalidHarborRunBundle, label, key)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return 0, fmt.Errorf("%w: %s.%s has trailing JSON", ErrInvalidHarborRunBundle, label, key)
	}
	number, numeric := decoded.(json.Number)
	if !numeric {
		return 0, fmt.Errorf("%w: %s.%s must be an integer", ErrInvalidHarborRunBundle, label, key)
	}
	value, err := number.Int64()
	if err != nil || value < 0 || int64(int(value)) != value {
		return 0, fmt.Errorf("%w: %s.%s must be a non-negative integer", ErrInvalidHarborRunBundle, label, key)
	}
	return int(value), nil
}

func harborRunBundleRequiredRFC3339Time(object map[string]json.RawMessage, key, label string) (time.Time, error) {
	value, err := harborRunBundleRequiredString(object, key, label)
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s.%s must be RFC3339: %v", ErrInvalidHarborRunBundle, label, key, err)
	}
	return parsed, nil
}

// harborRunBundleRequiredJobFinishedAtV018 accepts the two verified Harbor
// 0.18 job-result encodings. Trial timestamps remain RFC3339-only because
// they are actual cross-system instants. The job summary's naive datetime is
// retained as text rather than assigned an invented timezone.
func harborRunBundleRequiredJobFinishedAtV018(object map[string]json.RawMessage, key, label string) (string, error) {
	value, err := harborRunBundleRequiredString(object, key, label)
	if err != nil {
		return "", err
	}
	if _, parseErr := time.Parse(time.RFC3339Nano, value); parseErr == nil {
		return value, nil
	}
	if !harborRunBundleNaiveJobTimestampV018.MatchString(value) {
		return "", fmt.Errorf("%w: %s.%s must be RFC3339 or Harbor 0.18 naive ISO-8601", ErrInvalidHarborRunBundle, label, key)
	}
	if _, parseErr := time.Parse("2006-01-02T15:04:05", value); parseErr != nil {
		return "", fmt.Errorf("%w: %s.%s has an invalid Harbor 0.18 naive timestamp: %v", ErrInvalidHarborRunBundle, label, key, parseErr)
	}
	return value, nil
}

func harborRunBundleNumber(raw []byte, label string) (float64, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return 0, fmt.Errorf("%w: %s must be a JSON number", ErrInvalidHarborRunBundle, label)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return 0, fmt.Errorf("%w: %s has trailing JSON", ErrInvalidHarborRunBundle, label)
	}
	value, err := number.Float64()
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("%w: %s has an invalid number", ErrInvalidHarborRunBundle, label)
	}
	return value, nil
}

func (bundle HarborRunBundleV018) clone() HarborRunBundleV018 {
	bundle.Paths = append([]HarborRunBundlePathSummaryV018(nil), bundle.Paths...)
	bundle.Files = append([]HarborRunBundleFileV018(nil), bundle.Files...)
	return bundle
}

func cloneHarborRunBundleJobFacts(value HarborRunBundleJobFactsV018) HarborRunBundleJobFactsV018 {
	source := value.PassAtK
	value.PassAtK = make(map[string]map[string]float64, len(source))
	for group, metrics := range source {
		copyMetrics := make(map[string]float64, len(metrics))
		for k, metric := range metrics {
			copyMetrics[k] = metric
		}
		value.PassAtK[group] = copyMetrics
	}
	return value
}

func cloneHarborRunBundleTrials(values []HarborRunBundleTrialFactsV018) []HarborRunBundleTrialFactsV018 {
	copyValues := make([]HarborRunBundleTrialFactsV018, len(values))
	for index, value := range values {
		copyValues[index] = cloneHarborRunBundleTrialFacts(value)
	}
	return copyValues
}

func cloneHarborRunBundleTrialFacts(value HarborRunBundleTrialFactsV018) HarborRunBundleTrialFactsV018 {
	sourceRewards := value.VerifierRewards
	value.VerifierRewards = make(map[string]float64, len(sourceRewards))
	for key, reward := range sourceRewards {
		value.VerifierRewards[key] = reward
	}
	if value.Evaluator.ModelName != nil {
		modelName := *value.Evaluator.ModelName
		value.Evaluator.ModelName = &modelName
	}
	if value.Evaluator.ModelProvider != nil {
		provider := *value.Evaluator.ModelProvider
		value.Evaluator.ModelProvider = &provider
	}
	if value.TrajectoryTotalSteps != nil {
		steps := *value.TrajectoryTotalSteps
		value.TrajectoryTotalSteps = &steps
	}
	return value
}
