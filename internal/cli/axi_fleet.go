package cli

import (
	"fmt"
	"sort"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// fleetRow is one active or parked run anywhere on this machine. The columns
// are what a dashboard needs to place a run without opening it: where it lives,
// what it is doing, how recently it moved, and what it has published so far.
type fleetRow struct {
	Repo     string `toon:"repo"`
	Branch   string `toon:"branch"`
	Run      string `toon:"run"`
	Status   string `toon:"status"`
	Stage    string `toon:"stage"`
	Activity string `toon:"activity"`
	PR       string `toon:"pr"`
	Checks   string `toon:"checks"`
}

func newAxiFleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "List every active or parked run on this machine, across all repositories",
		Long: "Reports every pending, running, or parked pipeline run the local daemon\n" +
			"knows about, in every registered repository, so an observer discovers active\n" +
			"pipelines without a maintained list of project paths. A run launched from a\n" +
			"linked worktree is reported under its registered repository root.\n\n" +
			"This view is read-only. It never starts, answers, aborts, reruns, or\n" +
			"synchronizes a run, and it does not start the daemon; repository-scoped\n" +
			"commands such as `axi status` are unaffected by it.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAxiFleet(cmd)
		},
	}
	return cmd
}

// runAxiFleet renders the machine-wide view of active runs. Like the other
// read-only AXI queries it answers from the local database, so it works whether
// or not the daemon is up, needs no current repository, and emits no telemetry.
func runAxiFleet(cmd *cobra.Command) error {
	env, err := openAxiFleetEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	defer env.close()

	daemonState := "stopped"
	if alive, _ := daemon.IsRunning(env.p); alive {
		daemonState = "running"
	}

	repos, err := env.d.GetRepos()
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("list repositories: %v", err))
	}
	runs, err := env.d.GetActiveRuns()
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("list active runs: %v", err))
	}
	rows, reposWithRuns, err := fleetRows(env, repos, runs)
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}

	parked := 0
	for _, run := range runs {
		if run.AwaitingAgentSince != nil {
			parked++
		}
	}

	fields := []toon.Field{
		{Key: "scope", Value: "machine"},
		{Key: "daemon", Value: daemonState},
		{Key: "repositories", Value: len(repos)},
		{Key: "count", Value: fmt.Sprintf("%d active, %d parked, in %d of %d repositories", len(rows), parked, reposWithRuns, len(repos))},
	}
	if len(rows) == 0 {
		fields = append(fields, toon.Field{Key: "fleet", Value: "no active or parked runs on this machine"})
	} else {
		fields = append(fields, toon.Field{Key: "fleet", Value: rows})
	}
	fields = append(fields, toon.Field{Key: "help", Value: fleetHelp(daemonState, parked)})

	emitDoc(cmd, fields...)
	return nil
}

func fleetHelp(daemonState string, parked int) []string {
	help := []string{
		"This view is read-only and machine-wide: it never starts, answers, aborts, reruns, or synchronizes a run",
		"Run `no-mistakes axi status --run <id>` to inspect one listed run in detail; that selection is inspection-only",
	}
	if parked > 0 {
		help = append(help, "A parked run is waiting for its own driving agent, not stalled; answer its gate with `no-mistakes axi respond` from a worktree on that run's branch")
	}
	if daemonState != "running" {
		help = append(help, "The daemon is not running, so these rows are the last persisted state; it reconciles runs it no longer owns when it next starts")
	}
	return help
}

// fleetRows builds one row per active run, newest-first within each repository
// root so a dashboard can group by repository without re-sorting. It also
// returns how many distinct repositories are represented.
func fleetRows(env *axiEnv, repos []*db.Repo, runs []*db.Run) ([]fleetRow, int, error) {
	byID := make(map[string]*db.Repo, len(repos))
	for _, repo := range repos {
		byID[repo.ID] = repo
	}
	// GetActiveRuns already orders newest-first; a stable sort by repository
	// root keeps that order inside each group.
	ordered := make([]*db.Run, len(runs))
	copy(ordered, runs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return fleetRepoPath(byID, ordered[i]) < fleetRepoPath(byID, ordered[j])
	})

	rows := make([]fleetRow, 0, len(ordered))
	repoIDs := make(map[string]struct{}, len(ordered))
	for _, run := range ordered {
		steps, err := env.d.GetStepsByRun(run.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("load steps for run %s: %w", run.ID, err)
		}
		rv := runViewFromDB(run, steps, env.d)
		annotateRunView(env, &rv)
		rows = append(rows, fleetRow{
			Repo:     fleetRepoPath(byID, run),
			Branch:   run.Branch,
			Run:      run.ID,
			Status:   rv.Status,
			Stage:    fleetStage(rv),
			Activity: fleetActivity(rv),
			PR:       rv.PRURL,
			Checks:   fleetChecks(run, rv),
		})
		repoIDs[run.RepoID] = struct{}{}
	}
	return rows, len(repoIDs), nil
}

// fleetRepoPath is the registered repository root a run belongs to. A run
// launched from a linked worktree reports that same root, because worktrees
// resolve to their main repository when the run is recorded.
func fleetRepoPath(byID map[string]*db.Repo, run *db.Run) string {
	if repo, ok := byID[run.RepoID]; ok && repo != nil {
		return repo.WorkingPath
	}
	// An active run is never dropped from the fleet just because its
	// repository registration is gone. The marker keeps it visible and is
	// unmistakably not a path.
	return fmt.Sprintf("<unregistered repo %s>", run.RepoID)
}

// fleetStage names what the run is doing now: the gate it is parked at, else
// the step it is executing. A run that has not started a step yet reports
// nothing rather than naming a step that already finished.
func fleetStage(rv runView) string {
	if gate, ok := rv.awaitingStep(); ok {
		return gate.Name + ":" + gate.Status
	}
	if active := rv.activeRows(); len(active) > 0 {
		return active[0].Step + ":" + active[0].Status
	}
	return ""
}

// fleetActivity reports how recently the run moved: how long it has been parked
// when it is waiting for its driving agent, otherwise the active step's latest
// recorded activity.
func fleetActivity(rv runView) string {
	if rv.AwaitingAgentSince != nil && !terminalStatus(rv.Status) {
		return formatParkedFor(*rv.AwaitingAgentSince)
	}
	if active := rv.activeRows(); len(active) > 0 {
		return active[0].LastActivity
	}
	return ""
}

// fleetChecks reports the run's recorded check outcome. Only persisted
// readiness counts, so a run that has not reached CI reports nothing rather
// than an assumed pass.
func fleetChecks(run *db.Run, rv runView) string {
	if run.CIReadyAt != nil {
		if run.CIReadyNoCI {
			return "no-ci"
		}
		return "passed"
	}
	for _, step := range rv.Steps {
		if step.Name != string(types.StepCI) {
			continue
		}
		if step.Status == string(types.StepStatusRunning) || step.Status == string(types.StepStatusFixing) {
			return "monitoring"
		}
	}
	return ""
}
