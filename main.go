package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	tfe "github.com/hashicorp/go-tfe"
	"golang.org/x/exp/slices"
)

var ACTIONS = []string{"run", "confirm", "discard", "cancel", "cleanup"}

type Client struct {
	*tfe.Client
}

func main() {
	token := os.Getenv("TFE_TOKEN")
	if token == "" {
		fmt.Println("Environment variable 'TFE_TOKEN' not found")
		os.Exit(1)
	}

	org := flag.String("org", "", "Terraform Cloud organization name (required)")
	search := flag.String("search", "", "Workspace search (optional)")
	action := flag.String("action", "", "Action to do on the Workspace(s) [run|confirm|discard|cancel|cleanup] (required)")
	assume := flag.Bool("assume-yes", false, "Run without prompting for confirmation (optional)")
	stuckStatus := flag.String("stuck-status", "cost_estimated", "Where the Run waits for confirmation (optional; for cleanup only)")
	erroredOnly := flag.Bool("errored-only", false, "Only attempt the action if the current Run has Errored (optional; for run only)")

	flag.Parse()

	if *org == "" {
		flag.Usage()
		os.Exit(1)
	}

	if !slices.Contains(ACTIONS, *action) {
		flag.Usage()
		os.Exit(1)
	}

	client, err := newClient(token)
	if err != nil {
		slog.Error("Unable to create client", err)
		return
	}

	ctx := context.Background()

	start := time.Now()
	slog.Info("Running...")
	switch *action {
	case "run":
		client.Run(ctx, *org, *search, *assume, *erroredOnly)
	case "confirm":
		client.Confirm(ctx, *org, *search, *assume)
	case "discard":
		client.Discard(ctx, *org, *search, *assume)
	case "cancel":
		client.Cancel(ctx, *org, *search, *assume)
	case "cleanup":
		client.Cleanup(ctx, *org, *search, *assume, tfe.RunStatus(*stuckStatus))
	}
	slog.Info(fmt.Sprintf("Finished in %fs", time.Since(start).Seconds()))
}

func newClient(token string) (*Client, error) {
	config := &tfe.Config{
		Token: token,
	}

	client, err := tfe.NewClient(config)
	if err != nil {
		return &Client{}, err
	}

	return &Client{client}, nil
}

// Start a new Run if possible
func (c *Client) Run(ctx context.Context, org, search string, assume, erroredOnly bool) error {
	workspaces, err := c.getWorkspaces(ctx, org, search)
	if err != nil {
		return err
	}

	var createList []*tfe.Workspace
	for _, ws := range workspaces {
		if !erroredOnly || (erroredOnly && ws.CurrentRun.Status == tfe.RunErrored) {
			if ws.Permissions.CanQueueRun {
				slog.Info("can start", "workspace", ws.Name)
				createList = append(createList, ws)
			} else {
				slog.Warn("missing permission", "workspace", ws.Name)
			}
		}
	}

	if confirm(len(createList), assume) {
		for _, ws := range createList {
			if run, err := c.createRun(ctx, ws); err != nil {
				return err
			} else {
				slog.Info("started", "runID", run.ID)
			}
		}
	}

	return nil
}

// Confirm the CurrentRun if possible
func (c *Client) Confirm(ctx context.Context, org, search string, assume bool) error {
	workspaces, err := c.getWorkspaces(ctx, org, search)
	if err != nil {
		return err
	}

	var confirmList []string
	for _, ws := range workspaces {
		if c.canConfirm(ws.Name, ws.CurrentRun) {
			confirmList = append(confirmList, ws.CurrentRun.ID)
		}
	}

	if confirm(len(confirmList), assume) {
		return c.confirmRuns(ctx, confirmList)
	}

	return nil
}

// Discard the CurrentRun if possible
func (c *Client) Discard(ctx context.Context, org, search string, assume bool) error {
	workspaces, err := c.getWorkspaces(ctx, org, search)
	if err != nil {
		return err
	}

	var discardList []string
	for _, ws := range workspaces {
		if c.canDiscard(ws.Name, ws.CurrentRun) {
			discardList = append(discardList, ws.CurrentRun.ID)
		}
	}

	if confirm(len(discardList), assume) {
		return c.discardRuns(ctx, discardList)
	}

	return nil
}

// Cancel the CurrentRun if possible
func (c *Client) Cancel(ctx context.Context, org, search string, assume bool) error {
	workspaces, err := c.getWorkspaces(ctx, org, search)
	if err != nil {
		return err
	}

	var cancelList []string
	for _, ws := range workspaces {
		if c.canCancel(ws.Name, ws.CurrentRun) {
			cancelList = append(cancelList, ws.CurrentRun.ID)
		}
	}

	if confirm(len(cancelList), assume) {
		return c.cancelRuns(ctx, cancelList)
	}

	return nil
}

// Given one or more pending Run: confirm, cancel, or discard Runs until there are 1 or fewer Runs
func (c *Client) Cleanup(ctx context.Context, org, search string, assume bool, stuckStatus tfe.RunStatus) error {
	workspaces, err := c.getWorkspaces(ctx, org, search)
	if err != nil {
		return err
	}

	var (
		confirmList []string
		cancelList  []string
		discardList []string
		skipList    []string
	)

	for _, ws := range workspaces {
		if ws.CurrentRun.Status == stuckStatus {
			runs, err := c.getWaitingRuns(ctx, ws.ID, stuckStatus)
			if err != nil {
				return err
			}

			for idx, run := range runs {
				if idx == 0 {
					switch run.Status {
					case stuckStatus:
						if ws.AutoApply {
							if c.canConfirm(ws.Name, run) {
								confirmList = append(confirmList, run.ID)
							}
						} else {
							slog.Info("skipping, autoapply disabled", "workspace", ws.Name, "runID", run.ID)
						}
					case tfe.RunPending:
						// This one should queue automatically after cleanup
						slog.Info("will trigger automatically", "workspace", ws.Name, "runID", run.ID)
						skipList = append(skipList, run.ID)
					}
				} else {
					switch run.Status {
					case stuckStatus:
						if c.canDiscard(ws.Name, run) {
							discardList = append(discardList, run.ID)
						}
					case tfe.RunPending:
						if c.canCancel(ws.Name, run) {
							cancelList = append(cancelList, run.ID)
						}
					}
				}
			}
		}
	}

	changeCount := len(confirmList) + len(cancelList) + len(discardList) + len(skipList)
	if confirm(changeCount, assume) {
		// Cancel should happen before Discard
		if err := c.cancelRuns(ctx, cancelList); err != nil {
			return err
		}
		if err := c.discardRuns(ctx, discardList); err != nil {
			return err
		}
		if err := c.confirmRuns(ctx, confirmList); err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) getWaitingRuns(ctx context.Context, workspaceID string, stuckStatus tfe.RunStatus) ([]*tfe.Run, error) {
	var runs []*tfe.Run

	n := 0
	for {
		opts := &tfe.RunListOptions{
			ListOptions: tfe.ListOptions{
				PageNumber: n,
			},
		}

		runList, err := c.Runs.List(ctx, workspaceID, opts)
		if err != nil {
			return runs, err
		}

		for _, run := range runList.Items {
			if run.Status == stuckStatus || run.Status == tfe.RunPending {
				runs = append(runs, run)
			}
		}

		// Only continue if the last Run on the page is pending
		if len(runList.Items) > 0 && runList.Items[len(runList.Items)-1].Status == tfe.RunPending && runList.NextPage > n {
			n = runList.NextPage
		} else {
			return runs, nil
		}
	}
}

func (c *Client) createRun(ctx context.Context, workspace *tfe.Workspace) (*tfe.Run, error) {
	opts := tfe.RunCreateOptions{
		Workspace: workspace,
	}

	return c.Runs.Create(ctx, opts)
}

func (c *Client) canConfirm(name string, run *tfe.Run) bool {
	if run.Permissions.CanApply {
		if run.Actions.IsConfirmable {
			slog.Info("can confirm", "workspace", name, "runID", run.ID)
			return true
		} else {
			slog.Warn("not confirmable", "workspace", name, "runID", run.ID)
			return false
		}
	}

	slog.Warn("missing permission", "workspace", name, "runID", run.ID)
	return false
}

func (c *Client) confirmRuns(ctx context.Context, runIDs []string) error {
	for _, runID := range runIDs {
		if err := c.confirmRun(ctx, runID); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) confirmRun(ctx context.Context, runID string) error {
	slog.Info("confirming", "runID", runID)
	return c.Runs.Apply(ctx, runID, tfe.RunApplyOptions{})
}

func (c *Client) canCancel(name string, run *tfe.Run) bool {
	if run.Permissions.CanCancel {
		if run.Actions.IsCancelable {
			slog.Info("can cancel", "workspace", name, "runID", run.ID)
			return true
		} else {
			slog.Warn("not cancelable", "workspace", name, "runID", run.ID)
			return false
		}
	}

	slog.Warn("missing permission", "workspace", name, "runID", run.ID)
	return false
}

func (c *Client) cancelRuns(ctx context.Context, runIDs []string) error {
	for _, runID := range runIDs {
		if err := c.cancelRun(ctx, runID); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) cancelRun(ctx context.Context, runID string) error {
	slog.Info("canceling", "runID", runID)
	return c.Runs.Cancel(ctx, runID, tfe.RunCancelOptions{})
}

func (c *Client) canDiscard(name string, run *tfe.Run) bool {
	if run.Permissions.CanDiscard {
		if run.Actions.IsDiscardable {
			slog.Info("can discard", "workspace", name, "runID", run.ID)
			return true
		} else {
			slog.Warn("not discardable", "workspace", name, "runID", run.ID)
			return false
		}
	}

	slog.Warn("missing permission", "workspace", name, "runID", run.ID)
	return false
}

func (c *Client) discardRuns(ctx context.Context, runIDs []string) error {
	for _, runID := range runIDs {
		if err := c.discardRun(ctx, runID); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) discardRun(ctx context.Context, runID string) error {
	slog.Info("discarding", "runID", runID)
	return c.Runs.Discard(ctx, runID, tfe.RunDiscardOptions{})
}

func (c *Client) getWorkspaces(ctx context.Context, org, search string) ([]*tfe.Workspace, error) {
	var workspaces []*tfe.Workspace

	n := 0
	for {
		opts := &tfe.WorkspaceListOptions{
			ListOptions: tfe.ListOptions{
				PageNumber: n,
			},
			Search: search,
			Include: []tfe.WSIncludeOpt{
				"current_run",
			},
		}

		wsList, err := c.Workspaces.List(ctx, org, opts)
		if err != nil {
			return workspaces, err
		}

		for _, ws := range wsList.Items {
			if ws.CurrentRun != nil {
				workspaces = append(workspaces, ws)
			}
		}

		if wsList.NextPage > n {
			n = wsList.NextPage
		} else {
			slog.Info(fmt.Sprintf("Found %d Workspace(s)", len(workspaces)))
			return workspaces, nil
		}
	}
}

func confirm(changeCount int, assume bool) bool {
	if changeCount > 0 {
		if assume || confirmPrompt() {
			return true
		}
		slog.Info("Action(s) aborted")
	} else {
		slog.Info("Nothing to do")
	}

	return false
}

func confirmPrompt() bool {
	fmt.Print("Do you confirm the above action(s)? [y|N] ")

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return false
	}

	input = strings.TrimSuffix(input, "\n")

	if input == "y" || input == "yes" {
		return true
	}

	return false
}
